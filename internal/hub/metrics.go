package hub

// Metrics holds optional instrumentation callbacks.
// A nil field means that metric is not collected.
// Use NopMetrics() when instrumentation is not needed.
type Metrics struct {
	IncConnections       func()
	DecConnections       func()
	IncChannels          func()
	DecChannels          func()
	IncMessagesPublished func(channel string)
	AddMessagesPushed    func(channel string, n int)
	ObservePublish       func(channel string, d float64)
}

// NopMetrics returns a zero-value Metrics that silently discards all metrics.
func NopMetrics() Metrics {
	return Metrics{}
}
