package metrics

import (
	"github.com/aether-mq/aether/internal/hub"
	"github.com/prometheus/client_golang/prometheus"
)

// New creates Prometheus metrics and returns hub.Metrics callbacks that
// operate on them. All metrics are registered with the default prometheus
// registry so that the /metricsz endpoint exposes them automatically.
func New() hub.Metrics {
	connectionsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aether_connections_active",
		Help: "Current number of active WebSocket connections.",
	})
	channelsActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aether_channels_active",
		Help: "Current number of channels with at least one subscriber.",
	})
	messagesPublished := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aether_messages_published_total",
		Help: "Total number of messages published.",
	})
	messagesPushed := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aether_messages_pushed_total",
		Help: "Total number of message deliveries to subscribers.",
	})
	publishDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "aether_publish_duration_seconds",
		Help:    "End-to-end publish latency (write + fan-out) in seconds.",
		Buckets: prometheus.DefBuckets,
	})
	storageWriteDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "aether_storage_write_duration_seconds",
		Help:    "Storage write latency in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	prometheus.MustRegister(
		connectionsActive,
		channelsActive,
		messagesPublished,
		messagesPushed,
		publishDuration,
		storageWriteDuration,
	)

	return hub.Metrics{
		IncConnections: func() {
			connectionsActive.Inc()
		},
		DecConnections: func() {
			connectionsActive.Dec()
		},
		IncChannels: func() {
			channelsActive.Inc()
		},
		DecChannels: func() {
			channelsActive.Dec()
		},
		IncMessagesPublished: func(channel string) {
			messagesPublished.Inc()
		},
		AddMessagesPushed: func(channel string, n int) {
			messagesPushed.Add(float64(n))
		},
		ObservePublish: func(channel string, d float64) {
			publishDuration.Observe(d)
		},
		ObserveStorageWrite: func(channel string, d float64) {
			storageWriteDuration.Observe(d)
		},
	}
}
