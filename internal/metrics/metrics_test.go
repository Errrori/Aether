package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNewRateLimitRejected(t *testing.T) {
	onRejected := NewRateLimitRejected()
	onRejected("publisher")
	onRejected("publisher")
	onRejected("channel")

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, mf := range mfs {
		if mf.GetName() != "aether_rate_limited_total" {
			continue
		}
		got := map[string]float64{}
		for _, m := range mf.GetMetric() {
			scope := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "scope" {
					scope = lp.GetValue()
				}
			}
			got[scope] = m.GetCounter().GetValue()
		}
		if got["publisher"] != 2 {
			t.Errorf("publisher rejections = %v, want 2", got["publisher"])
		}
		if got["channel"] != 1 {
			t.Errorf("channel rejections = %v, want 1", got["channel"])
		}
		return
	}

	t.Fatal("aether_rate_limited_total not registered")
}
