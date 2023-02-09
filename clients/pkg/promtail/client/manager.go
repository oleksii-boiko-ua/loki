package client

import (
	"fmt"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"strings"
	"sync"

	"github.com/grafana/loki/clients/pkg/promtail/api"
	"github.com/grafana/loki/clients/pkg/promtail/wal"
)

type Stoppable interface {
	Stop()
}

// Manager manages remote write client instantiation, and connects the related components to orchestrate the flow of api.Entry
// from the scrape targets, to the remote write clients themselves.
//
// Right now it just supports instantiating the WAL writer side of the future-to-be WAL enabled client. In follow-up
// work, tracked in https://github.com/grafana/loki/issues/8197, this Manager will be responsible for instantiating all client
// types: Logger, Multi and WAL.
type Manager struct {
	clients     []Client
	walWatchers []Stoppable

	entries chan api.Entry
	once    sync.Once

	wg sync.WaitGroup
}

// NewManager creates a new Manager
func NewManager(metrics *Metrics, logger log.Logger, maxStreams, maxLineSize int, maxLineSizeTruncate bool, reg prometheus.Registerer, walCfg wal.Config, clientCfgs ...Config) (*Manager, error) {
	// TODO: refactor this to instantiate all clients types
	var fake struct{}

	watcherMetrics := wal.NewWatcherMetrics(reg)

	if len(clientCfgs) == 0 {
		return nil, fmt.Errorf("at least one client config should be provided")
	}
	clientsCheck := make(map[string]struct{})
	clients := make([]Client, 0, len(clientCfgs))
	watchers := make([]Stoppable, 0, len(clientCfgs))
	for _, cfg := range clientCfgs {
		client, err := New(metrics, cfg, maxStreams, maxLineSize, maxLineSizeTruncate, logger)
		if err != nil {
			return nil, err
		}

		// Don't allow duplicate clients, we have client specific metrics that need at least one unique label value (name).
		if _, ok := clientsCheck[client.Name()]; ok {
			return nil, fmt.Errorf("duplicate client configs are not allowed, found duplicate for name: %s", cfg.Name)
		}

		clientsCheck[client.Name()] = fake
		clients = append(clients, client)

		// look for deletes segments every 1/2 the max segment age, that way we are not generating too much noise on the write
		// to, and we allow a maximum series cache drift of max segment age / 2.
		// Create and launch wal watcher for this client
		watcher := wal.NewWatcher(walCfg.Dir, client.Name(), watcherMetrics, newClientWriteTo(client.Chan(), logger), logger, walCfg.MaxSegmentAge/2)
		watcher.Start()
		watchers = append(watchers, watcher)
	}

	manager := &Manager{
		clients: clients,
		entries: make(chan api.Entry),
	}
	manager.start()
	return manager, nil
}

func (m *Manager) start() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// keep reading received entries
		for range m.entries {
			// then fanout to every remote write client
			//for _, c := range m.clients {
			//	c.Chan() <- e
			//}
		}
	}()
}

func (m *Manager) StopNow() {
	for _, c := range m.clients {
		c.StopNow()
	}
}

func (m *Manager) Name() string {
	var sb strings.Builder
	// name contains wal since manager is used as client only when WAL enabled for now
	sb.WriteString("wal:")
	for i, c := range m.clients {
		sb.WriteString(c.Name())
		if i != len(m.clients)-1 {
			sb.WriteString(",")
		}
	}
	return sb.String()
}

func (m *Manager) Chan() chan<- api.Entry {
	return m.entries
}

func (m *Manager) Stop() {
	// first stop the receiving channel
	m.once.Do(func() { close(m.entries) })
	m.wg.Wait()
	// close wal watchers
	for _, walWatcher := range m.walWatchers {
		walWatcher.Stop()
	}
	// close clients
	for _, c := range m.clients {
		c.Stop()
	}
}
