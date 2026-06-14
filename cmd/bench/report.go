package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// LatencyStats holds percentile-based latency statistics.
type LatencyStats struct {
	Min   time.Duration `json:"min_ms"`
	Max   time.Duration `json:"max_ms"`
	Avg   time.Duration `json:"avg_ms"`
	P50   time.Duration `json:"p50_ms"`
	P90   time.Duration `json:"p90_ms"`
	P99   time.Duration `json:"p99_ms"`
	P99_9 time.Duration `json:"p99_9_ms"`
	Count int           `json:"count"`
}

// ComputeLatencyStats computes percentile statistics from a slice of durations.
func ComputeLatencyStats(latencies []time.Duration) LatencyStats {
	if len(latencies) == 0 {
		return LatencyStats{}
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	return LatencyStats{
		Min:   sorted[0],
		Max:   sorted[len(sorted)-1],
		Avg:   sum / time.Duration(len(sorted)),
		P50:   percentile(sorted, 50),
		P90:   percentile(sorted, 90),
		P99:   percentile(sorted, 99),
		P99_9: percentile(sorted, 99.9),
		Count: len(sorted),
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := p / 100.0 * float64(len(sorted)-1)
	idx := int(rank)
	frac := rank - float64(idx)
	if idx >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[idx] + time.Duration(frac*float64(sorted[idx+1]-sorted[idx]))
}

// ScenarioResult holds the results of a single benchmark scenario.
type ScenarioResult struct {
	Name              string        `json:"name"`
	Duration          time.Duration `json:"duration_sec"`
	Connections       int           `json:"connections,omitempty"`
	PublishQPS        float64       `json:"publish_qps"`
	PushQPS           float64       `json:"push_qps,omitempty"`
	MemoryMB          float64       `json:"memory_mb,omitempty"`
	CacheHitRate      float64       `json:"cache_hit_rate_pct,omitempty"`
	PublishLatency    LatencyStats  `json:"publish_latency"`
	EndToEndLatency   LatencyStats  `json:"end_to_end_latency"`
	HistoryReplaySec  float64       `json:"history_replay_sec,omitempty"`
	HistoryReplayRate float64       `json:"history_replay_msg_per_sec,omitempty"`
}

// OutputResults writes results as JSON and prints a human-readable summary.
func OutputResults(results []ScenarioResult, path string) error {
	// JSON output.
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write results: %w", err)
	}

	// Human-readable summary.
	fmt.Println("\n========== Benchmark Results ==========")
	for _, r := range results {
		fmt.Printf("\n--- %s ---\n", r.Name)
		if r.Connections > 0 {
			fmt.Printf("  Connections:       %d\n", r.Connections)
		}
		fmt.Printf("  Publish QPS:       %.1f\n", r.PublishQPS)
		if r.PushQPS > 0 {
			fmt.Printf("  Push QPS:          %.1f\n", r.PushQPS)
		}
		if r.MemoryMB > 0 {
			fmt.Printf("  Memory:            %.1f MB\n", r.MemoryMB)
		}
		if r.CacheHitRate > 0 {
			fmt.Printf("  Cache Hit Rate:    %.1f%%\n", r.CacheHitRate)
		}
		if r.HistoryReplaySec > 0 {
			fmt.Printf("  History Replay:    %.3f s (%.0f msg/s)\n", r.HistoryReplaySec, r.HistoryReplayRate)
		}
		if r.EndToEndLatency.Count > 0 {
			fmt.Printf("  E2E Latency (ms):  P50=%.2f P90=%.2f P99=%.2f P99.9=%.2f\n",
				float64(r.EndToEndLatency.P50)/float64(time.Millisecond),
				float64(r.EndToEndLatency.P90)/float64(time.Millisecond),
				float64(r.EndToEndLatency.P99)/float64(time.Millisecond),
				float64(r.EndToEndLatency.P99_9)/float64(time.Millisecond),
			)
		}
		if r.PublishLatency.Count > 0 {
			fmt.Printf("  Pub Latency (ms):  P50=%.2f P90=%.2f P99=%.2f\n",
				float64(r.PublishLatency.P50)/float64(time.Millisecond),
				float64(r.PublishLatency.P90)/float64(time.Millisecond),
				float64(r.PublishLatency.P99)/float64(time.Millisecond),
			)
		}
	}
	fmt.Println("\n=======================================")
	fmt.Printf("Results saved to: %s\n", path)
	return nil
}
