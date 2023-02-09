package wal

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"

	"github.com/grafana/loki/pkg/ingester/wal"
)

const (
	readPeriod         = 10 * time.Millisecond
	segmentCheckPeriod = 100 * time.Millisecond
)

// Based in the implementation of prometheus WAL watcher
// https://github.com/prometheus/prometheus/blob/main/tsdb/wlog/watcher.go. Includes some changes to make it suitable
// for log WAL entries, but also, the writeTo surface has been implemented according to the actions necessary for
// Promtail's WAL.

// Reader is a dependency interface to inject generic WAL readers into the Watcher.
type Reader interface {
	Next() bool
	Err() error
	Record() []byte
}

// WriteTo is responsible for doing the necessary work to process both series and entries while the Watcher
// is reading / tailing segments. Note that StoreSeries and SeriesReset might be called concurrently.
//
// Based on https://github.com/prometheus/prometheus/blob/main/tsdb/wlog/watcher.go#L46
type WriteTo interface {
	// StoreSeries is called when series are found in WAL entries by the watcher, alongside with the segmentNum they were
	// found in.
	StoreSeries(series []record.RefSeries, segmentNum int)

	AppendEntries(entries wal.RefEntries) error

	// SeriesReset is called when the Watcher notices that segments have been deleted. The argument of the call
	// signifies that all segments with a number lower or equal than segmentNum are safe to be reclaimed.
	SeriesReset(segmentNum int)
}

type Watcher struct {
	// id identifies the Watcher. Used when one Watcher is instantiated per remote write client, to be able to track to whom
	// the metric/log line corresponds.
	id string

	writeTo                         WriteTo
	done                            chan struct{}
	quit                            chan struct{}
	walDir                          string
	logger                          log.Logger
	MaxSegment                      int
	seenSegments                    diffset
	deletedSegmentsWatcherFrequency time.Duration

	metrics *WatcherMetrics
}

// NewWatcher creates a new Watcher.
func NewWatcher(walDir, id string, metrics *WatcherMetrics, writeTo WriteTo, logger log.Logger, deletedSegmentsWatcherFrequency time.Duration) *Watcher {
	return &Watcher{
		walDir:                          walDir,
		id:                              id,
		writeTo:                         writeTo,
		quit:                            make(chan struct{}),
		done:                            make(chan struct{}),
		MaxSegment:                      -1,
		deletedSegmentsWatcherFrequency: deletedSegmentsWatcherFrequency,
		seenSegments:                    diffset{},
		logger:                          logger,
		metrics:                         metrics,
	}
}

// Start runs the watcher main loop.
func (w *Watcher) Start() {
	w.metrics.watchersRunning.WithLabelValues().Inc()
	go w.mainLoop()
	go w.runSeriesResetWatcher()
}

// mainLoop retries when there's an error reading a specific segment or advancing one, but leaving a bigger time in-between
// retries.
func (w *Watcher) mainLoop() {
	defer close(w.done)
	for !isClosed(w.quit) {
		if err := w.run(); err != nil {
			level.Error(w.logger).Log("msg", "error tailing WAL", "err", err)
		}

		select {
		case <-w.quit:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *Watcher) runSeriesResetWatcher() {
	ticker := time.NewTicker(w.deletedSegmentsWatcherFrequency)
	for {
		select {
		case <-ticker.C:
			// run series reset
			if err := w.readSegmentsAndEmitSeriesResets(); err != nil {
				level.Error(w.logger).Log("msg", "error emitting series resets", "err", err)
			}
		case <-w.quit:
			// closing
			ticker.Stop()
			return
		}
	}
}

func (w *Watcher) readSegmentsAndEmitSeriesResets() error {
	segments, err := readSegmentNumbers(w.walDir)
	if err != nil {
		return err
	}
	newSeenSegments := newDiffset(segments)
	diff := w.seenSegments.Difference(newSeenSegments)
	w.seenSegments = newSeenSegments
	if len(diff) == 0 {
		level.Debug(w.logger).Log("msg", "No segment was gc-ed. No series being resetted")
		return nil
	}
	// Since segments are created with a segment number that is increasing, we can order them by it's number like
	// segment i, segment i+1, ..., segment n. Also, we can assume that if segment i was last written at time T, all
	// segments before were last written at a time T', with T' < T.
	// Promtail only has a single WAL writer, therefore at each time, the WAL has a single writer head. Also, this writer
	// creates a routine that deletes segments older than a configured threshold. Considering the above, we can assure that
	// if at some point a segment i was deleted, all segments numbered j with j <= i are deleted or safe to delete as well.
	// Therefore, if SeriesReset is called with the highest numbered deleted segment in the diffset, then all previous
	// were as well.
	maxDeleted := -1
	for s := range diff {
		if s > maxDeleted {
			maxDeleted = s
		}
	}
	w.writeTo.SeriesReset(maxDeleted)
	return nil
}

// Run the watcher, which will tail the WAL until the quit channel is closed
// or an error case is hit.
func (w *Watcher) run() error {
	_, lastSegment, err := w.firstAndLast()
	if err != nil {
		return fmt.Errorf("wal.Segments: %w", err)
	}

	currentSegment := lastSegment
	level.Debug(w.logger).Log("msg", "Tailing WAL", "currentSegment", currentSegment, "lastSegment", lastSegment)
	for !isClosed(w.quit) {
		w.metrics.currentSegment.WithLabelValues(w.id).Set(float64(currentSegment))
		level.Debug(w.logger).Log("msg", "Processing segment", "currentSegment", currentSegment)

		// On start, we have a pointer to what is the latest segment. On subsequent calls to this function,
		// currentSegment will have been incremented, and we should open that segment.
		if err := w.watch(currentSegment); err != nil {
			return err
		}

		// For testing: stop when you hit a specific segment.
		if currentSegment == w.MaxSegment {
			return nil
		}

		currentSegment++
	}

	return nil
}

// watch will start reading from the segment identified by segmentNum. If an EOF is reached, it will keep
// reading for more WAL records with a wlog.LiveReader. Periodically, it will check if there's a new segment, and if positive
// read the remaining from the current one and return.
func (w *Watcher) watch(segmentNum int) error {
	segment, err := wlog.OpenReadSegment(wlog.SegmentName(w.walDir, segmentNum))
	if err != nil {
		return err
	}
	defer segment.Close()

	reader := wlog.NewLiveReader(w.logger, nil, segment)

	readTicker := time.NewTicker(readPeriod)
	defer readTicker.Stop()

	segmentTicker := time.NewTicker(segmentCheckPeriod)
	defer segmentTicker.Stop()

	for {
		select {
		case <-w.quit:
			return nil

		case <-segmentTicker.C:
			_, last, err := w.firstAndLast()
			if err != nil {
				return fmt.Errorf("segments: %w", err)
			}

			// Check if new segments exists.
			if last <= segmentNum {
				continue
			}

			// Since we know last > segmentNum, there must be a new segment. Read the remaining from the segmentNum segment
			// and return from `watch` to read the next one
			err = w.readSegment(reader, segmentNum)

			// When we are tailing, non-EOFs are fatal.
			if errors.Cause(err) != io.EOF {
				return err
			}

			return nil

		case <-readTicker.C:
			err = w.readSegment(reader, segmentNum)

			// Otherwise, when we are tailing, non-EOFs are fatal.
			if errors.Cause(err) != io.EOF {
				return err
			}
		}
	}
}

// Read entries from a segment, decode them and dispatch them.
func (w *Watcher) readSegment(r *wlog.LiveReader, segmentNum int) error {
	for r.Next() && !isClosed(w.quit) {
		b := r.Record()
		if err := r.Err(); err != nil {
			return err
		}
		w.metrics.recordsRead.WithLabelValues(w.id).Inc()

		if err := w.decodeAndDispatch(b, segmentNum); err != nil {
			return err
		}
	}
	return errors.Wrapf(r.Err(), "segment %d: %v", segmentNum, r.Err())
}

// decodeAndDispatch first decodes a WAL record. Upon reading either Series or Entries from the WAL record, call the
// appropriate callbacks in the writeTo.
func (w *Watcher) decodeAndDispatch(b []byte, segmentNum int) error {
	rec := recordPool.GetRecord()
	if err := wal.DecodeRecord(b, rec); err != nil {
		w.metrics.recordDecodeFails.WithLabelValues(w.id).Inc()
		return err
	}

	// First process all series to ensure we don't write entries to non-existent series.
	var firstErr error
	w.writeTo.StoreSeries(rec.Series, segmentNum)

	for _, entries := range rec.RefEntries {
		if err := w.writeTo.AppendEntries(entries); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (w *Watcher) Stop() {
	// first close the quit channel to order main mainLoop routine to stop
	close(w.quit)
	// upon calling stop, wait for main mainLoop execution to stop
	<-w.done

	w.metrics.watchersRunning.WithLabelValues().Dec()
}

// firstAndList finds the first and last segment number for a WAL directory.
func (w *Watcher) firstAndLast() (int, int, error) {
	refs, err := readSegmentNumbers(w.walDir)
	if err != nil {

		return -1, -1, err
	}

	if len(refs) == 0 {
		return -1, -1, nil
	}

	// Start with sentinel values and walk back to the first and last (min and max)
	var first = math.MaxInt32
	var last = -1
	for _, segmentReg := range refs {
		if segmentReg < first {
			first = segmentReg
		}
		if segmentReg > last {
			last = segmentReg
		}
	}
	return first, last, nil
}

// isClosed checks in a non-blocking manner if a channel is closed or not.
func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// readSegmentNumbers reads the given directory and returns all segment identifiers, that is, the index of each segment
// file.
func readSegmentNumbers(dir string) ([]int, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var refs []int
	for _, f := range files {
		k, err := strconv.Atoi(f.Name())
		if err != nil {
			continue
		}
		refs = append(refs, k)
	}
	return refs, nil
}
