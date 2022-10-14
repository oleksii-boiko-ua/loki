package retention

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	statusFailure  = "failure"
	statusSuccess  = "success"
	statusNotFound = "notfound"

	tableActionModified = "modified"
	tableActionDeleted  = "deleted"
	tableActionNone     = "none"
)

type sweeperMetrics struct {
	deleteChunkDurationSeconds *prometheus.HistogramVec
	markerFileCurrentTime      prometheus.Gauge
	markerFilesCurrent         prometheus.Gauge
	markerFilesDeletedTotal    prometheus.Counter
}

func newSweeperMetrics(objectType string, r prometheus.Registerer) *sweeperMetrics {
	constLabels := prometheus.Labels{
		objectType: objectType,
	}

	return &sweeperMetrics{
		deleteChunkDurationSeconds: promauto.With(r).NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_sweeper_chunk_deleted_duration_seconds",
			Help:        "Time (in seconds) spent in deleting chunk",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(0.1, 2, 8),
		}, []string{"status"}),
		markerFilesCurrent: promauto.With(r).NewGauge(prometheus.GaugeOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_sweeper_marker_files_current",
			Help:        "The current total of marker files valid for deletion.",
			ConstLabels: constLabels,
		}),
		markerFileCurrentTime: promauto.With(r).NewGauge(prometheus.GaugeOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_sweeper_marker_file_processing_current_time",
			Help:        "The current time of creation of the marker file being processed.",
			ConstLabels: constLabels,
		}),
		markerFilesDeletedTotal: promauto.With(r).NewCounter(prometheus.CounterOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_sweeper_marker_files_deleted_total",
			Help:        "The total of marker files deleted after being fully processed.",
			ConstLabels: constLabels,
		}),
	}
}

type markerMetrics struct {
	tableProcessedTotal           *prometheus.CounterVec
	tableMarksCreatedTotal        *prometheus.CounterVec
	tableProcessedDurationSeconds *prometheus.HistogramVec
}

func newMarkerMetrics(objectType string, r prometheus.Registerer) *markerMetrics {
	constLabels := prometheus.Labels{
		objectType: objectType,
	}

	return &markerMetrics{
		tableProcessedTotal: promauto.With(r).NewCounterVec(prometheus.CounterOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_marker_table_processed_total",
			Help:        "Total amount of table processed for each user per action. Empty string for user_id is for common index",
			ConstLabels: constLabels,
		}, []string{"table", "user_id", "action"}),
		tableMarksCreatedTotal: promauto.With(r).NewCounterVec(prometheus.CounterOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_marker_count_total",
			Help:        "Total count of markers created per table.",
			ConstLabels: constLabels,
		}, []string{"table"}),
		tableProcessedDurationSeconds: promauto.With(r).NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "loki_boltdb_shipper",
			Name:        "retention_marker_table_processed_duration_seconds",
			Help:        "Time (in seconds) spent in marking table for chunks to delete",
			ConstLabels: constLabels,
			Buckets:     []float64{1, 2.5, 5, 10, 20, 40, 90, 360, 600, 1800},
		}, []string{"table", "status"}),
	}
}
