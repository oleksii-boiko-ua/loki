package file

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/loki/clients/pkg/promtail/api"
	"github.com/grafana/loki/clients/pkg/promtail/positions"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"go.uber.org/atomic"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

type decompresser struct {
	metrics   *Metrics
	logger    log.Logger
	handler   api.EntryHandler
	positions positions.Positions

	path string

	posAndSizeMtx sync.Mutex
	stopOnce      sync.Once

	running *atomic.Bool
	posquit chan struct{}
	posdone chan struct{}
	done    chan struct{}

	decoder *encoding.Decoder

	compressionReader io.Reader
	compressionBuf    *bytes.Buffer

	position int64
	size     int64
}

func newDecompresser(metrics *Metrics, logger log.Logger, handler api.EntryHandler, positions positions.Positions, path string, encodingFormat string) (*decompresser, error) {
	logger = log.With(logger, "component", "decompresser")

	fi, err := os.Stat(path)
	if err != nil {
		return nil, errors.Wrap(err, "os stat")
	}

	pos, err := positions.Get(path)
	if err != nil {
		return nil, errors.Wrap(err, "get positions")
	}

	if fi.Size() < pos {
		positions.Remove(path)
	}

	compressionReader, err := mountReader(path, logger)
	if err != nil {
		return nil, errors.Wrap(err, "mount reader")
	}

	var decoder *encoding.Decoder
	if encodingFormat != "" {
		level.Info(logger).Log("msg", "decompresser will decode messages", "from", encodingFormat, "to", "UTF8")
		encoder, err := ianaindex.IANA.Encoding(encodingFormat)
		if err != nil {
			return nil, errors.Wrap(err, "error doing IANA encoding")
		}
		decoder = encoder.NewDecoder()
	}

	decompresser := &decompresser{
		metrics:           metrics,
		logger:            logger,
		handler:           api.AddLabelsMiddleware(model.LabelSet{FilenameLabel: model.LabelValue(path)}).Wrap(handler),
		positions:         positions,
		path:              path,
		running:           atomic.NewBool(false),
		posquit:           make(chan struct{}),
		posdone:           make(chan struct{}),
		done:              make(chan struct{}),
		compressionReader: compressionReader,
		decoder:           decoder,
	}

	go decompresser.readLines()
	go decompresser.updatePosition()
	metrics.filesActive.Add(1.)
	return decompresser, nil
}

// mountReader instantiate a reader ready to be used by the decompresser.
//
// The selected reader implementation is based on the extension of the given file name.
// It'll error if the extension isn't supported.
func mountReader(path string, logger log.Logger) (reader io.Reader, err error) {
	ext := filepath.Ext(path)
	var decompressLib string

	if strings.Contains(ext, "gz") { // .gz, .tar.gz
		decompressLib = "compress/gzip"
		reader, err = gzip.NewReader(&bytes.Buffer{})
	} else if ext == "z" {
		decompressLib = "compress/zlib"
		reader, err = zlib.NewReader(&bytes.Buffer{})
	} else if ext == "zip" {
		decompressLib = "compress/flate"
		reader = flate.NewReader(&bytes.Buffer{})
	} else if ext == "bz2" {
		decompressLib = "bzip2"
		reader = bzip2.NewReader(&bytes.Buffer{})
	}

	level.Info(logger).Log("msg", fmt.Sprintf("using %q to decompress file %q", decompressLib, path))

	if reader != nil && err == nil {
		return
	}

	if err != nil {
		return nil, err
	}

	return nil, fmt.Errorf("file %q with unsupported extension", path)
}

func (t *decompresser) updatePosition() {
	positionSyncPeriod := t.positions.SyncPeriod()
	positionWait := time.NewTicker(positionSyncPeriod)
	defer func() {
		positionWait.Stop()
		level.Info(t.logger).Log("msg", "position timer: exited", "path", t.path)
		close(t.posdone)
	}()

	for {
		select {
		case <-positionWait.C:
			if err := t.MarkPositionAndSize(); err != nil {
				level.Error(t.logger).Log("msg", "position timer: error getting position and/or size, stopping decompresser", "path", t.path, "error", err)
				return
			}
		case <-t.posquit:
			return
		}
	}
}

// readLines read all existing lines of the given compressed file.
//
// It first decompress the file as a whole using a reader and then it will iterate
// over its chunks, separated by '\n'.
// During each iteration, the parsed and decoded log line is then sent to the API with the current timestamp.
func (t *decompresser) readLines() {
	level.Info(t.logger).Log("msg", "read lines routine: started", "path", t.path)

	t.running.Store(true)

	defer func() {
		t.cleanupMetrics()
		t.running.Store(false)
		level.Info(t.logger).Log("msg", "read lines routine finished", "path", t.path)
		close(t.done)
	}()
	entries := t.handler.Chan()

	content, err := os.ReadFile(t.path)
	if err != nil {
		level.Error(t.logger).Log("msg", "error reading file", "path", t.path, "error", err)
		return
	}

	if _, err = t.compressionReader.Read(content); err != nil {
		level.Error(t.logger).Log("msg", "error reading line", "path", t.path, "error", err)
		return
	}

	level.Info(t.logger).Log("msg", "successfully decompressed file", "path", t.path)

	var buf *bytes.Buffer
	io.Copy(buf, t.compressionReader)
	decompressedText := buf.String()

	decompressedTextReader := strings.NewReader(decompressedText)
	bufReader := bufio.NewReader(decompressedTextReader)

	// iterate over decompressed file, decode and send lines to API.
	for {
		s, err := bufReader.ReadString('\n')
		var text string
		if t.decoder != nil {
			var err error
			text, err = t.convertToUTF8(s)
			if err != nil {
				level.Debug(t.logger).Log("msg", "failed to convert encoding", "error", err)
				t.metrics.encodingFailures.WithLabelValues(t.path).Inc()
				text = fmt.Sprintf("the requested encoding conversion for this line failed in Promtail/Grafana Agent: %s", err.Error())
			}
		} else {
			text = s
		}

		t.metrics.readLines.WithLabelValues(t.path).Inc()
		entries <- api.Entry{
			Labels: model.LabelSet{},
			Entry: logproto.Entry{
				Timestamp: time.Now(),
				Line:      text,
			},
		}

		t.size = int64(bufReader.Size())
		t.position += 1

		if err != nil {
			break
		}
	}
}

func (t *decompresser) MarkPositionAndSize() error {
	// Lock this update as there are 2 timers calling this routine, the sync in filetarget and the positions sync in this file.
	t.posAndSizeMtx.Lock()
	defer t.posAndSizeMtx.Unlock()

	t.metrics.totalBytes.WithLabelValues(t.path).Set(float64(t.size))
	t.metrics.readBytes.WithLabelValues(t.path).Set(float64(t.position))
	t.positions.Put(t.path, t.position)

	return nil
}

func (t *decompresser) Stop() {
	// stop can be called by two separate threads in filetarget, to avoid a panic closing channels more than once
	// we wrap the stop in a sync.Once.
	t.stopOnce.Do(func() {
		// Shut down the position marker thread
		close(t.posquit)
		<-t.posdone

		// Save the current position before shutting down tailer
		if err := t.MarkPositionAndSize(); err != nil {
			level.Error(t.logger).Log("msg", "error marking file position when stopping decompresser", "path", t.path, "error", err)
		}

		// Wait for readLines() to consume all the remaining messages and exit when the channel is closed
		<-t.done
		level.Info(t.logger).Log("msg", "stopped decompresser", "path", t.path)
		t.handler.Stop()
		t.compressionBuf.Reset()
	})
}

func (t *decompresser) IsRunning() bool {
	return t.running.Load()
}

func (t *decompresser) convertToUTF8(text string) (string, error) {
	res, _, err := transform.String(t.decoder, text)
	if err != nil {
		return "", errors.Wrap(err, "error decoding text")
	}

	return res, nil
}

// cleanupMetrics removes all metrics exported by this tailer
func (t *decompresser) cleanupMetrics() {
	// When we stop tailing the file, also un-export metrics related to the file
	t.metrics.filesActive.Add(-1.)
	t.metrics.readLines.DeleteLabelValues(t.path)
	t.metrics.readBytes.DeleteLabelValues(t.path)
	t.metrics.totalBytes.DeleteLabelValues(t.path)
}

func (t *decompresser) Path() string {
	return t.path
}
