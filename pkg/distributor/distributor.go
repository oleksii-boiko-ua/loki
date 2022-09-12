package distributor

import (
	"context"
	"flag"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log/level"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/dskit/limiter"
	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/tenant"
	lru "github.com/hashicorp/golang-lru"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"

	"github.com/grafana/loki/pkg/distributor/clientpool"
	"github.com/grafana/loki/pkg/ingester/client"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/runtime"
	"github.com/grafana/loki/pkg/storage/stores/indexshipper/compactor/retention"
	"github.com/grafana/loki/pkg/usagestats"
	"github.com/grafana/loki/pkg/util"
	util_log "github.com/grafana/loki/pkg/util/log"
	"github.com/grafana/loki/pkg/validation"
)

const (
	// ShardLbName is the internal label to be used by Loki when dividing a stream into smaller pieces.
	// Possible values are only increasing integers starting from 0.
	ShardLbName        = "__stream_shard__"
	ShardLbPlaceholder = "__placeholder__"

	ringKey = "distributor"
)

var (
	maxLabelCacheSize = 100000
	rfStats           = usagestats.NewInt("distributor_replication_factor")
)

type ShardStreamsConfig struct {
	Enabled        bool `yaml:"enabled"`
	LoggingEnabled bool `yaml:"logging_enabled"`
}

// Config for a Distributor.
type Config struct {
	// Distributors ring
	DistributorRing RingConfig `yaml:"ring,omitempty"`

	// For testing.
	factory ring_client.PoolFactory `yaml:"-"`

	// ShardStreams configures wether big streams should be sharded or not.
	ShardStreams ShardStreamsConfig `yaml:"shard_streams"`
}

// RegisterFlags registers distributor-related flags.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	cfg.DistributorRing.RegisterFlags(fs)
	fs.BoolVar(&cfg.ShardStreams.Enabled, "distributor.stream-sharding.enabled", false, "Automatically shard streams to keep them under the per-stream rate limit")
	fs.BoolVar(&cfg.ShardStreams.LoggingEnabled, "distributor.stream-sharding.logging-enabled", false, "Enable logging when sharding streams")
}

// StreamSharder manages the state necessary to shard streams.
type StreamSharder interface {
	ShardCountFor(stream logproto.Stream) (int, bool)
	IncreaseShardsFor(stream logproto.Stream)
}

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	services.Service

	cfg              Config
	clientCfg        client.Config
	tenantConfigs    *runtime.TenantConfigs
	tenantsRetention *retention.TenantsRetention
	ingestersRing    ring.ReadRing
	validator        *Validator
	pool             *ring_client.Pool
	streamSharder    StreamSharder

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances.
	distributorsLifecycler *ring.Lifecycler

	rateLimitStrat string

	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher
	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter
	labelCache           *lru.Cache
	// metrics
	ingesterAppends        *prometheus.CounterVec
	ingesterAppendFailures *prometheus.CounterVec
	replicationFactor      prometheus.Gauge
}

// New a distributor creates.
func New(
	cfg Config,
	clientCfg client.Config,
	configs *runtime.TenantConfigs,
	ingestersRing ring.ReadRing,
	overrides *validation.Overrides,
	registerer prometheus.Registerer,
) (*Distributor, error) {
	factory := cfg.factory
	if factory == nil {
		factory = func(addr string) (ring_client.PoolClient, error) {
			return client.New(clientCfg, addr)
		}
	}

	validator, err := NewValidator(overrides)
	if err != nil {
		return nil, err
	}

	// Create the configured ingestion rate limit strategy (local or global).
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorsLifecycler *ring.Lifecycler
	rateLimitStrat := validation.LocalIngestionRateStrategy

	var servs []services.Service
	if overrides.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
		rateLimitStrat = validation.GlobalIngestionRateStrategy
		if err != nil {
			return nil, errors.Wrap(err, "create distributor KV store client")
		}

		distributorsLifecycler, err = ring.NewLifecycler(cfg.DistributorRing.ToLifecyclerConfig(), nil, "distributor", ringKey, false, util_log.Logger, prometheus.WrapRegistererWithPrefix("cortex_", registerer))
		if err != nil {
			return nil, errors.Wrap(err, "create distributor lifecycler")
		}

		servs = append(servs, distributorsLifecycler)
		ingestionRateStrategy = newGlobalIngestionRateStrategy(overrides, distributorsLifecycler)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(overrides)
	}

	labelCache, err := lru.New(maxLabelCacheSize)
	if err != nil {
		return nil, err
	}
	d := Distributor{
		cfg:                    cfg,
		clientCfg:              clientCfg,
		tenantConfigs:          configs,
		tenantsRetention:       retention.NewTenantsRetention(overrides),
		ingestersRing:          ingestersRing,
		distributorsLifecycler: distributorsLifecycler,
		validator:              validator,
		pool:                   clientpool.NewPool(clientCfg.PoolConfig, ingestersRing, factory, util_log.Logger),
		ingestionRateLimiter:   limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second),
		labelCache:             labelCache,
		rateLimitStrat:         rateLimitStrat,
		ingesterAppends: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: "loki",
			Name:      "distributor_ingester_appends_total",
			Help:      "The total number of batch appends sent to ingesters.",
		}, []string{"ingester"}),
		ingesterAppendFailures: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: "loki",
			Name:      "distributor_ingester_append_failures_total",
			Help:      "The total number of failed batch appends sent to ingesters.",
		}, []string{"ingester"}),
		replicationFactor: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Namespace: "loki",
			Name:      "distributor_replication_factor",
			Help:      "The configured replication factor.",
		}),
	}
	d.replicationFactor.Set(float64(ingestersRing.ReplicationFactor()))
	rfStats.Set(int64(ingestersRing.ReplicationFactor()))

	servs = append(servs, d.pool)
	d.subservices, err = services.NewManager(servs...)
	if err != nil {
		return nil, errors.Wrap(err, "services manager")
	}
	d.subservicesWatcher = services.NewFailureWatcher()
	d.subservicesWatcher.WatchManager(d.subservices)
	d.Service = services.NewBasicService(d.starting, d.running, d.stopping)

	d.streamSharder = NewStreamSharder()

	return &d, nil
}

func (d *Distributor) starting(ctx context.Context) error {
	return services.StartManagerAndAwaitHealthy(ctx, d.subservices)
}

func (d *Distributor) running(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-d.subservicesWatcher.Chan():
		return errors.Wrap(err, "distributor subservice failed")
	}
}

func (d *Distributor) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type streamTracker struct {
	stream      logproto.Stream
	minSuccess  int
	maxFailures int
	succeeded   atomic.Int32
	failed      atomic.Int32
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type pushTracker struct {
	samplesPending atomic.Int32
	samplesFailed  atomic.Int32
	done           chan struct{}
	err            chan error
}

// Push a set of streams.
// The returned error is the last one seen.
func (d *Distributor) Push(ctx context.Context, req *logproto.PushRequest) (*logproto.PushResponse, error) {
	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}

	// Return early if request does not contain any streams
	if len(req.Streams) == 0 {
		return &logproto.PushResponse{}, nil
	}

	keys, streams, validationErr, ok := d.validateStreams(req.Streams, userID)
	if !ok {
		return nil, validationErr
	}

	if len(streams) == 0 {
		return &logproto.PushResponse{}, validationErr
	}

	tracker, err := d.pushStreams(ctx, keys, streams, userID)
	if err != nil {
		return nil, err
	}

	select {
	case err := <-tracker.err:
		return nil, err
	case <-tracker.done:
		return &logproto.PushResponse{}, validationErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *Distributor) validateStreams(streams []logproto.Stream, userID string) ([]uint32, []streamTracker, error, bool) {
	// First we flatten out the request into a list of samples.
	// We use the heuristic of 1 sample per TS to size the array.
	// We also work out the hash value at the same time.
	validStreams := make([]streamTracker, 0, len(streams))
	keys := make([]uint32, 0, len(streams))
	validatedSamplesSize := 0
	validatedSamplesCount := 0

	var validationErr error
	validationContext := d.validator.getValidationContextForTime(time.Now(), userID)

	for _, stream := range streams {
		// Return early if stream does not contain any entries
		if len(stream.Entries) == 0 {
			continue
		}

		// Truncate first so subsequent steps have consistent line lengths
		d.truncateLines(validationContext, &stream)

		var err error
		stream.Labels, err = d.parseStreamLabels(validationContext, stream.Labels, &stream)
		if err != nil {
			validationErr = err
			validation.DiscardedSamples.WithLabelValues(validation.InvalidLabels, userID).Add(float64(len(stream.Entries)))
			bytes := 0
			for _, e := range stream.Entries {
				bytes += len(e.Line)
			}
			validation.DiscardedBytes.WithLabelValues(validation.InvalidLabels, userID).Add(float64(bytes))
			continue
		}

		n := 0
		for _, entry := range stream.Entries {
			if err := d.validator.ValidateEntry(validationContext, stream.Labels, entry); err != nil {
				validationErr = err
				continue
			}

			stream.Entries[n] = entry

			// If configured for this tenant, increment duplicate timestamps. Note, this is imperfect
			// since Loki will accept out of order writes it doesn't account for separate
			// pushes with overlapping time ranges having entries with duplicate timestamps
			if validationContext.incrementDuplicateTimestamps && n != 0 {
				// Traditional logic for Loki is that 2 lines with the same timestamp and
				// exact same content will be de-duplicated, (i.e. only one will be stored, others dropped)
				// To maintain this behavior, only increment the timestamp if the log content is different
				if stream.Entries[n-1].Line != entry.Line {
					stream.Entries[n].Timestamp = maxT(entry.Timestamp, stream.Entries[n-1].Timestamp.Add(1*time.Nanosecond))
				}
			}

			n++
			validatedSamplesSize += len(entry.Line)
			validatedSamplesCount++
		}
		stream.Entries = stream.Entries[:n]

		// TODO: Shard somewhere else
		if d.cfg.ShardStreams.Enabled {
			derivedKeys, derivedStreams := d.shardStream(stream, userID)
			keys = append(keys, derivedKeys...)
			validStreams = append(validStreams, derivedStreams...)
		} else {
			keys = append(keys, util.TokenFor(userID, stream.Labels))
			validStreams = append(validStreams, streamTracker{stream: stream})
		}
	}

	// Return early if none of the validStreams contained entries
	if len(validStreams) == 0 {
		return keys, validStreams, validationErr, true
	}

	now := time.Now()
	if !d.ingestionRateLimiter.AllowN(now, userID, validatedSamplesSize) {
		// Return a 429 to indicate to the client they are being rate limited
		validation.DiscardedSamples.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedSamplesCount))
		validation.DiscardedBytes.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedSamplesSize))
		return nil, nil, httpgrpc.Errorf(http.StatusTooManyRequests, validation.RateLimitedErrorMsg, userID, int(d.ingestionRateLimiter.Limit(now, userID)), validatedSamplesCount, validatedSamplesSize), false
	}

	return keys, validStreams, validationErr, true
}

func (d *Distributor) pushStreams(ctx context.Context, keys []uint32, streams []streamTracker, userID string) (pushTracker, error) {
	const maxExpectedReplicationSet = 5 // typical replication factor 3 plus one for inactive plus one for luck
	var descs [maxExpectedReplicationSet]ring.InstanceDesc

	samplesByIngester := map[string][]*streamTracker{}
	ingesterDescs := map[string]ring.InstanceDesc{}
	for i, key := range keys {
		replicationSet, err := d.ingestersRing.Get(key, ring.Write, descs[:0], nil, nil)
		if err != nil {
			return pushTracker{}, err
		}

		streams[i].minSuccess = len(replicationSet.Instances) - replicationSet.MaxErrors
		streams[i].maxFailures = replicationSet.MaxErrors
		for _, ingester := range replicationSet.Instances {
			samplesByIngester[ingester.Addr] = append(samplesByIngester[ingester.Addr], &streams[i])
			ingesterDescs[ingester.Addr] = ingester
		}
	}

	tracker := pushTracker{
		done: make(chan struct{}, 1), // buffer avoids blocking if caller terminates - sendSamples() only sends once on each
		err:  make(chan error, 1),
	}

	tracker.samplesPending.Store(int32(len(streams)))
	for ingester, samples := range samplesByIngester {
		go func(ingester ring.InstanceDesc, samples []*streamTracker) {
			// Use a background context to make sure all ingesters get samples even if we return early
			localCtx, cancel := context.WithTimeout(context.Background(), d.clientCfg.RemoteTimeout)
			defer cancel()
			localCtx = user.InjectOrgID(localCtx, userID)
			if sp := opentracing.SpanFromContext(ctx); sp != nil {
				localCtx = opentracing.ContextWithSpan(localCtx, sp)
			}
			d.sendSamples(localCtx, ingester, samples, &tracker)
		}(ingesterDescs[ingester], samples)
	}

	return tracker, nil
}

func min(x1, x2 int) int {
	if x1 < x2 {
		return x1
	}
	return x2
}

// shardStream shards (divides) the given stream into N smaller streams, where
// N is the sharding size for the given stream. shardSteam returns the smaller
// streams and their associated keys for hashing to ingesters.
func (d *Distributor) shardStream(stream logproto.Stream, userID string) ([]uint32, []streamTracker) {
	shardCount, ok := d.streamSharder.ShardCountFor(stream)
	if !ok || shardCount <= 1 {
		return []uint32{util.TokenFor(userID, stream.Labels)}, []streamTracker{{stream: stream}}
	}

	if d.cfg.ShardStreams.LoggingEnabled {
		level.Info(util_log.Logger).Log("msg", "sharding request", "stream", stream.Labels)
	}

	streamLabels := labelTemplate(stream.Labels)
	streamPattern := streamLabels.String()

	derivedKeys := make([]uint32, 0, shardCount)
	derivedStreams := make([]streamTracker, 0, shardCount)
	for i := 0; i < shardCount; i++ {
		shard, ok := d.createShard(stream, streamLabels, streamPattern, shardCount, i)
		if !ok {
			continue
		}

		derivedKeys = append(derivedKeys, util.TokenFor(userID, shard.Labels))
		derivedStreams = append(derivedStreams, streamTracker{stream: shard})

		if d.cfg.ShardStreams.LoggingEnabled {
			level.Info(util_log.Logger).Log("msg", "stream derived from sharding", "src-stream", stream.Labels, "derived-stream", shard.Labels)
		}
	}

	return derivedKeys, derivedStreams
}

// labelTemplate returns a label set that includes the dummy label to be replaced
// To avoid allocations, this slice is reused when we know the stream value
func labelTemplate(lbls string) labels.Labels {
	baseLbls, err := syntax.ParseLabels(lbls)
	if err != nil {
		level.Error(util_log.Logger).Log("msg", "couldn't extract labels from stream", "stream", lbls)
		return nil
	}

	streamLabels := make([]labels.Label, len(baseLbls)+1)
	for i := 0; i < len(baseLbls); i++ {
		streamLabels[i] = baseLbls[i]
	}
	streamLabels[len(baseLbls)] = labels.Label{Name: ShardLbName, Value: ShardLbPlaceholder}

	return streamLabels
}

func (d *Distributor) createShard(stream logproto.Stream, lbls labels.Labels, streamPattern string, totalShards, shardNumber int) (logproto.Stream, bool) {
	lowerBound, upperBound, ok := d.boundsFor(stream, totalShards, shardNumber)
	if !ok {
		return logproto.Stream{}, false
	}

	shardLabel := strconv.Itoa(shardNumber)
	lbls[len(lbls)-1] = labels.Label{Name: ShardLbName, Value: shardLabel}
	return logproto.Stream{
		Labels:  strings.Replace(streamPattern, ShardLbPlaceholder, shardLabel, 1),
		Hash:    lbls.Hash(),
		Entries: stream.Entries[lowerBound:upperBound],
	}, true
}

func (d *Distributor) boundsFor(stream logproto.Stream, totalShards, shardNumber int) (int, int, bool) {
	entriesPerWindow := float64(len(stream.Entries)) / float64(totalShards)

	fIdx := float64(shardNumber)
	lowerBound := int(fIdx * entriesPerWindow)
	upperBound := min(int(entriesPerWindow*(1+fIdx)), len(stream.Entries))

	if lowerBound > upperBound {
		if d.cfg.ShardStreams.LoggingEnabled {
			level.Warn(util_log.Logger).Log("msg", "sharding with lowerbound > upperbound", "lowerbound", lowerBound, "upperbound", upperBound, "shards", totalShards, "labels", stream.Labels)
		}
		return 0, 0, false
	}

	return lowerBound, upperBound, true
}

// maxT returns the highest between two given timestamps.
func maxT(t1, t2 time.Time) time.Time {
	if t1.Before(t2) {
		return t2
	}

	return t1
}

func (d *Distributor) truncateLines(vContext validationContext, stream *logproto.Stream) {
	if !vContext.maxLineSizeTruncate {
		return
	}

	var truncatedSamples, truncatedBytes int
	for i, e := range stream.Entries {
		if maxSize := vContext.maxLineSize; maxSize != 0 && len(e.Line) > maxSize {
			stream.Entries[i].Line = e.Line[:maxSize]

			truncatedSamples++
			truncatedBytes = len(e.Line) - maxSize
		}
	}

	validation.MutatedSamples.WithLabelValues(validation.LineTooLong, vContext.userID).Add(float64(truncatedSamples))
	validation.MutatedBytes.WithLabelValues(validation.LineTooLong, vContext.userID).Add(float64(truncatedBytes))
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamples(ctx context.Context, ingester ring.InstanceDesc, streamTrackers []*streamTracker, pushTracker *pushTracker) {
	err := d.sendSamplesErr(ctx, ingester, streamTrackers)

	// If we succeed, decrement each sample's pending count by one.  If we reach
	// the required number of successful puts on this sample, then decrement the
	// number of pending samples by one.  If we successfully push all samples to
	// min success ingesters, wake up the waiting rpc so it can return early.
	// Similarly, track the number of errors, and if it exceeds maxFailures
	// shortcut the waiting rpc.
	//
	// The use of atomic increments here guarantees only a single sendSamples
	// goroutine will write to either channel.
	for i := range streamTrackers {
		if err != nil {
			if streamTrackers[i].failed.Inc() <= int32(streamTrackers[i].maxFailures) {
				continue
			}
			if pushTracker.samplesFailed.Inc() == 1 {
				pushTracker.err <- err
			}
		} else {
			if streamTrackers[i].succeeded.Inc() != int32(streamTrackers[i].minSuccess) {
				continue
			}
			if pushTracker.samplesPending.Dec() == 0 {
				pushTracker.done <- struct{}{}
			}
		}
	}
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamplesErr(ctx context.Context, ingester ring.InstanceDesc, streams []*streamTracker) error {
	c, err := d.pool.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}

	req := &logproto.PushRequest{
		Streams: make([]logproto.Stream, len(streams)),
	}
	for i, s := range streams {
		req.Streams[i] = s.stream
	}

	_, err = c.(logproto.PusherClient).Push(ctx, req)
	d.ingesterAppends.WithLabelValues(ingester.Addr).Inc()
	if err != nil {
		d.ingesterAppendFailures.WithLabelValues(ingester.Addr).Inc()
	}
	return err
}

func (d *Distributor) parseStreamLabels(vContext validationContext, key string, stream *logproto.Stream) (string, error) {
	labelVal, ok := d.labelCache.Get(key)
	if ok {
		return labelVal.(string), nil
	}
	ls, err := syntax.ParseLabels(key)
	if err != nil {
		return "", httpgrpc.Errorf(http.StatusBadRequest, validation.InvalidLabelsErrorMsg, key, err)
	}
	// ensure labels are correctly sorted.
	if err := d.validator.ValidateLabels(vContext, ls, *stream); err != nil {
		return "", err
	}
	lsVal := ls.String()
	d.labelCache.Add(key, lsVal)
	return lsVal, nil
}
