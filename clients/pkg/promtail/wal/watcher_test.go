package wal

import (
	"os"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/clients/pkg/promtail/api"
	"github.com/grafana/loki/pkg/ingester/wal"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/util"
)

type testWriteTo struct {
	ReadEntries []api.Entry
	series      map[uint64]model.LabelSet
	logger      log.Logger
}

func (t *testWriteTo) StoreSeries(series []record.RefSeries, i int) {
	for _, seriesRec := range series {
		t.series[uint64(seriesRec.Ref)] = util.MapToModelLabelSet(seriesRec.Labels.Map())
	}
}

func (t *testWriteTo) AppendEntries(entries wal.RefEntries) error {
	var entry api.Entry
	if l, ok := t.series[uint64(entries.Ref)]; ok {
		entry.Labels = l
		for _, e := range entries.Entries {
			entry.Entry = e
			t.ReadEntries = append(t.ReadEntries, entry)
		}
	} else {
		level.Debug(t.logger).Log("series for entry not found")
	}
	return nil
}

// watcherTestResources contains all resources necessary to test an individual Watcher functionality
type watcherTestResources struct {
	writeEntry     func(entry api.Entry)
	startWatcher   func()
	syncWAL        func() error
	nextWALSegment func() error
	writeTo        *testWriteTo
}

type watcherTest func(t *testing.T, res *watcherTestResources)

// cases defines the watcher test cases
var cases = map[string]watcherTest{
	"read entries from WAL": func(t *testing.T, res *watcherTestResources) {
		res.startWatcher()

		lines := []string{
			"holis",
			"holus",
			"chau",
		}
		testLabels := model.LabelSet{
			"test": "watcher_read",
		}

		for _, line := range lines {
			res.writeEntry(api.Entry{
				Labels: testLabels,
				Entry: logproto.Entry{
					Timestamp: time.Now(),
					Line:      line,
				},
			})
		}
		require.NoError(t, res.syncWAL())

		require.Eventually(t, func() bool {
			return len(res.writeTo.ReadEntries) == 3
		}, time.Second*10, time.Second, "expected watcher to catch up with written entries")
		for _, readEntry := range res.writeTo.ReadEntries {
			require.Contains(t, lines, readEntry.Line, "not expected log line")
		}
	},

	"continue reading entries in next segment after initial segment is closed": func(t *testing.T, res *watcherTestResources) {
		res.startWatcher()
		lines := []string{
			"holis",
			"holus",
			"chau",
		}
		linesAfter := []string{
			"holis2",
			"holus2",
			"chau2",
		}
		testLabels := model.LabelSet{
			"test": "watcher_read",
		}

		for _, line := range lines {
			res.writeEntry(api.Entry{
				Labels: testLabels,
				Entry: logproto.Entry{
					Timestamp: time.Now(),
					Line:      line,
				},
			})
		}
		require.NoError(t, res.syncWAL())

		require.Eventually(t, func() bool {
			return len(res.writeTo.ReadEntries) == 3
		}, time.Second*10, time.Second, "expected watcher to catch up with written entries")
		for _, readEntry := range res.writeTo.ReadEntries {
			require.Contains(t, lines, readEntry.Line, "not expected log line")
		}

		err := res.nextWALSegment()
		require.NoError(t, err, "expected no error when moving to next wal segment")

		for _, line := range linesAfter {
			res.writeEntry(api.Entry{
				Labels: testLabels,
				Entry: logproto.Entry{
					Timestamp: time.Now(),
					Line:      line,
				},
			})
		}
		require.NoError(t, res.syncWAL())

		require.Eventually(t, func() bool {
			return len(res.writeTo.ReadEntries) == 6
		}, time.Second*10, time.Second, "expected watcher to catch up after new wal segment is cut")
		// assert over second half of entries
		for _, readEntry := range res.writeTo.ReadEntries[3:] {
			require.Contains(t, linesAfter, readEntry.Line, "not expected log line")
		}
	},

	"start reading from last segment": func(t *testing.T, res *watcherTestResources) {
		linesAfter := []string{
			"holis2",
			"holus2",
			"chau2",
		}
		testLabels := model.LabelSet{
			"test": "watcher_read",
		}

		// write something to first segment
		res.writeEntry(api.Entry{
			Labels: testLabels,
			Entry: logproto.Entry{
				Timestamp: time.Now(),
				Line:      "this shouldn't be read",
			},
		})

		require.NoError(t, res.syncWAL())

		err := res.nextWALSegment()
		require.NoError(t, err, "expected no error when moving to next wal segment")

		res.startWatcher()

		for _, line := range linesAfter {
			res.writeEntry(api.Entry{
				Labels: testLabels,
				Entry: logproto.Entry{
					Timestamp: time.Now(),
					Line:      line,
				},
			})
		}
		require.NoError(t, res.syncWAL())

		require.Eventually(t, func() bool {
			return len(res.writeTo.ReadEntries) == 3
		}, time.Second*10, time.Second, "expected watcher to catch up after new wal segment is cut")
		// assert over second half of entries
		for _, readEntry := range res.writeTo.ReadEntries[3:] {
			require.Contains(t, linesAfter, readEntry.Line, "not expected log line")
		}
	},
}

// TestWatcher is the main test function, that works as framework to test different scenarios of the Watcher. It bootstraps
// necessary test components.
func TestWatcher(t *testing.T) {
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			// start test global resources
			reg := prometheus.NewRegistry()
			logger := level.NewFilter(log.NewLogfmtLogger(os.Stdout), level.AllowDebug())
			dir := t.TempDir()
			metrics := NewWatcherMetrics(reg)
			writeTo := &testWriteTo{
				series: map[uint64]model.LabelSet{},
				logger: logger,
			}
			// create new watcher, and defer stop
			watcher := NewWatcher(dir, "test", metrics, writeTo, logger)
			defer watcher.Stop()
			wl, err := New(Config{
				Enabled: true,
				Dir:     dir,
			}, logger, reg)
			require.NoError(t, err)
			ew := newEntryWriter()
			// run test case injecting resources
			testCase(
				t,
				&watcherTestResources{
					writeEntry: func(entry api.Entry) {
						ew.WriteEntry(entry, wl, logger)
					},
					startWatcher: func() {
						watcher.Start()
					},
					syncWAL: func() error {
						return wl.Sync()
					},
					nextWALSegment: func() error {
						_, err := wl.NextSegment()
						return err
					},
					writeTo: writeTo,
				},
			)
		})
	}
}
