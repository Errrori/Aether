package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type benchConfig struct {
	server    string
	apiKey    string
	jwtSecret string
	scenario  string
	duration  time.Duration
	output    string
}

func main() {
	cfg := parseFlags()

	fmt.Printf("Hardware: %d CPU cores, Go %s\n\n", runtime.NumCPU(), runtime.Version())

	var results []ScenarioResult

	switch cfg.scenario {
	case "a":
		results = append(results, runScenarioA(cfg))
	case "b":
		results = append(results, runScenarioB(cfg)...)
	case "c":
		results = append(results, runScenarioC(cfg))
	case "d":
		results = append(results, runScenarioD(cfg)...)
	case "all":
		results = append(results, runScenarioA(cfg))
		results = append(results, runScenarioB(cfg)...)
		results = append(results, runScenarioC(cfg))
		results = append(results, runScenarioD(cfg)...)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s (valid: a, b, c, d, all)\n", cfg.scenario)
		os.Exit(1)
	}

	if err := OutputResults(results, cfg.output); err != nil {
		fmt.Fprintf(os.Stderr, "output results: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() benchConfig {
	cfg := benchConfig{}
	flag.StringVar(&cfg.server, "server", "http://localhost:8080", "Aether server address")
	flag.StringVar(&cfg.apiKey, "api-key", "", "API key for publish auth (required)")
	flag.StringVar(&cfg.jwtSecret, "jwt-secret", "", "JWT signing secret (required, >=32 bytes)")
	flag.StringVar(&cfg.scenario, "scenario", "all", "Scenario to run: a, b, c, d, all")
	flag.DurationVar(&cfg.duration, "duration", 10*time.Second, "Duration per scenario variant")
	flag.StringVar(&cfg.output, "output", "bench_e2e_results.json", "Output file path")
	flag.Parse()

	if cfg.apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: --api-key is required")
		os.Exit(1)
	}
	if cfg.jwtSecret == "" {
		fmt.Fprintln(os.Stderr, "error: --jwt-secret is required")
		os.Exit(1)
	}
	if len(cfg.jwtSecret) < 32 {
		fmt.Fprintln(os.Stderr, "error: --jwt-secret must be at least 32 bytes")
		os.Exit(1)
	}
	return cfg
}

// --- scenario A: max concurrent connections ---

func runScenarioA(cfg benchConfig) ScenarioResult {
	fmt.Println("\n=== Scenario A: Max Concurrent Connections ===")

	const batchSize = 500
	const batchInterval = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var totalConns atomic.Int64
	var totalFailures atomic.Int64
	var activeSubs sync.WaitGroup

	go func() {
		for batch := 1; ; batch++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var batchWg sync.WaitGroup
			for i := 0; i < batchSize; i++ {
				batchWg.Add(1)
				go func(idx int) {
					defer batchWg.Done()

					sub := NewSubscriber(cfg.server, cfg.jwtSecret)
					subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
					defer subCancel()

					if err := sub.Connect(subCtx, fmt.Sprintf("bench-a-%d-%d", batch, idx)); err != nil {
						totalFailures.Add(1)
						return
					}
					if err := sub.Subscribe(subCtx, []string{fmt.Sprintf("bench.a.%d.%d", batch, idx)}, nil); err != nil {
						totalFailures.Add(1)
						sub.Close()
						return
					}

					totalConns.Add(1)
					// Add to activeSubs before spawning goroutine: per CLAUDE.md,
					// "Add 必须与资源注册到对外可见的数据结构在同一临界区内完成".
					activeSubs.Add(1)
					go func() {
						defer activeSubs.Done()
						defer sub.Close()
						sub.ReadLoop(ctx)
					}()
				}(i)
			}

			batchWg.Wait()

			conns := totalConns.Load()
			fails := totalFailures.Load()
			memMB := readServerMemory(cfg.server)
			fmt.Printf("  Batch %d: %d connections, %d failures, %.0f MB memory\n", batch, conns, fails, memMB)

			if fails > int64(float64(conns)*0.01) && conns > 1000 {
				fmt.Printf("  Failure rate exceeded 1%%, stopping\n")
				cancel()
				return
			}

			if memMB > 2048 {
				fmt.Printf("  Memory exceeded 2GB, stopping\n")
				cancel()
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(batchInterval):
			}
		}
	}()

	// Let it run with timeout.
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Minute):
		cancel()
	}

	time.Sleep(2 * time.Second)

	conns := totalConns.Load()
	memMB := readServerMemory(cfg.server)
	cacheRate := readCacheHitRate(cfg.server)

	cancel()
	activeSubs.Wait()

	return ScenarioResult{
		Name:         "A - Max Concurrent Connections",
		Connections:  int(conns),
		MemoryMB:     memMB,
		CacheHitRate: cacheRate,
	}
}

// --- scenario B: throughput at various fan-out sizes ---

type scenarioBVariants struct {
	name        string
	channel     string
	subscribers int
}

func runScenarioB(cfg benchConfig) []ScenarioResult {
	fmt.Println("\n=== Scenario B: Publish Throughput ===")

	variants := []scenarioBVariants{
		{"B-a: 1 channel + 1 subscriber", "bench.b.a", 1},
		{"B-b: 1 channel + 100 subscribers", "bench.b.b", 100},
		{"B-c: 100 channels x 1 subscriber", "", 0},
		{"B-d: 1 channel + 1000 subscribers", "bench.b.d", 1000},
	}

	var results []ScenarioResult

	for _, v := range variants {
		fmt.Printf("\n  Running %s for %v...\n", v.name, cfg.duration)
		var r ScenarioResult
		if v.subscribers == 0 {
			r = runScenarioBMultiChannel(cfg, 100)
		} else {
			r = runScenarioBSingle(cfg, v)
		}
		r.Name = v.name
		results = append(results, r)
	}

	return results
}

func runScenarioBSingle(cfg benchConfig, v scenarioBVariants) ScenarioResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var subs []*Subscriber
	var subWg sync.WaitGroup
	var allE2E []time.Duration
	var e2eMu sync.Mutex

	for i := 0; i < v.subscribers; i++ {
		sub := NewSubscriber(cfg.server, cfg.jwtSecret)
		subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := sub.Connect(subCtx, fmt.Sprintf("bench-b-%d", i)); err != nil {
			fmt.Printf("    subscriber %d connect: %v\n", i, err)
			subCancel()
			continue
		}
		if err := sub.Subscribe(subCtx, []string{v.channel}, nil); err != nil {
			fmt.Printf("    subscriber %d subscribe: %v\n", i, err)
			subCancel()
			sub.Close()
			continue
		}
		subCancel()

		subs = append(subs, sub)
		subWg.Add(1)
		go func(s *Subscriber) {
			defer subWg.Done()
			defer s.Close()
			_ = s.ReadLoop(ctx)
			_, latencies := s.Stats()
			e2eMu.Lock()
			allE2E = append(allE2E, latencies...)
			e2eMu.Unlock()
		}(sub)
	}

	time.Sleep(500 * time.Millisecond)
	fmt.Printf("    %d subscribers connected, publishing...\n", len(subs))

	pub := NewPublisher(cfg.server, cfg.apiKey)
	var pubWg sync.WaitGroup
	pubCtx, pubCancel := context.WithTimeout(ctx, cfg.duration)
	defer pubCancel()

	pubWg.Add(1)
	go func() {
		defer pubWg.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
			}
			payload := benchPayload()
			pub.Publish(ctx, v.channel, payload)
		}
	}()

	pubWg.Wait()
	cancel()
	subWg.Wait()

	successes, failures, pubLatencies := pub.Stats()
	totalMessages := successes + failures
	actualDuration := cfg.duration.Seconds()
	if actualDuration == 0 {
		actualDuration = 1
	}
	publishQPS := float64(totalMessages) / actualDuration
	pushQPS := float64(len(allE2E)) / actualDuration

	return ScenarioResult{
		Connections:     len(subs),
		PublishQPS:      publishQPS,
		PushQPS:         pushQPS,
		PublishLatency:  ComputeLatencyStats(pubLatencies),
		EndToEndLatency: ComputeLatencyStats(allE2E),
	}
}

func runScenarioBMultiChannel(cfg benchConfig, numChannels int) ScenarioResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var subs []*Subscriber
	var subWg sync.WaitGroup
	var allE2E []time.Duration
	var e2eMu sync.Mutex

	for i := 0; i < numChannels; i++ {
		ch := fmt.Sprintf("bench.b.c.%d", i)
		sub := NewSubscriber(cfg.server, cfg.jwtSecret)
		subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := sub.Connect(subCtx, fmt.Sprintf("bench-b-c-%d", i)); err != nil {
			subCancel()
			continue
		}
		if err := sub.Subscribe(subCtx, []string{ch}, nil); err != nil {
			subCancel()
			sub.Close()
			continue
		}
		subCancel()

		subs = append(subs, sub)
		subWg.Add(1)
		go func(s *Subscriber) {
			defer subWg.Done()
			defer s.Close()
			_ = s.ReadLoop(ctx)
			_, latencies := s.Stats()
			e2eMu.Lock()
			allE2E = append(allE2E, latencies...)
			e2eMu.Unlock()
		}(sub)
	}

	time.Sleep(500 * time.Millisecond)
	fmt.Printf("    %d subscribers across %d channels, publishing...\n", len(subs), numChannels)

	pub := NewPublisher(cfg.server, cfg.apiKey)
	var pubWg sync.WaitGroup
	pubCtx, pubCancel := context.WithTimeout(ctx, cfg.duration)
	defer pubCancel()

	var chIdx atomic.Int64
	pubWg.Add(1)
	go func() {
		defer pubWg.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
			}
			ch := fmt.Sprintf("bench.b.c.%d", chIdx.Add(1)%int64(numChannels))
			pub.Publish(ctx, ch, benchPayload())
		}
	}()

	pubWg.Wait()
	cancel()
	subWg.Wait()

	successes, failures, pubLatencies := pub.Stats()
	totalMessages := successes + failures
	actualDuration := cfg.duration.Seconds()
	if actualDuration == 0 {
		actualDuration = 1
	}

	return ScenarioResult{
		Connections:     len(subs),
		PublishQPS:      float64(totalMessages) / actualDuration,
		PushQPS:         float64(len(allE2E)) / actualDuration,
		PublishLatency:  ComputeLatencyStats(pubLatencies),
		EndToEndLatency: ComputeLatencyStats(allE2E),
	}
}

// --- scenario C: stability test ---

func runScenarioC(cfg benchConfig) ScenarioResult {
	fmt.Println("\n=== Scenario C: Stability Test (5 min) ===")

	const targetConns = 500
	const targetMsgPerSec = 500
	const totalDuration = 5 * time.Minute
	const sampleInterval = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), totalDuration+30*time.Second)
	defer cancel()

	var subs []*Subscriber
	var subWg sync.WaitGroup

	for i := 0; i < targetConns; i++ {
		sub := NewSubscriber(cfg.server, cfg.jwtSecret)
		subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := sub.Connect(subCtx, fmt.Sprintf("bench-c-%d", i)); err != nil {
			subCancel()
			continue
		}
		if err := sub.Subscribe(subCtx, []string{"bench.c"}, nil); err != nil {
			subCancel()
			sub.Close()
			continue
		}
		subCancel()

		subs = append(subs, sub)
		subWg.Add(1)
		go func(s *Subscriber) {
			defer subWg.Done()
			defer s.Close()
			_ = s.ReadLoop(ctx)
		}(sub)
	}

	fmt.Printf("  %d subscribers connected\n", len(subs))

	pub := NewPublisher(cfg.server, cfg.apiKey)
	var pubWg sync.WaitGroup
	pubCtx, pubCancel := context.WithCancel(ctx)
	defer pubCancel()

	interval := time.Second / time.Duration(targetMsgPerSec)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	pubWg.Add(1)
	go func() {
		defer pubWg.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			case <-ticker.C:
				pub.Publish(ctx, "bench.c", benchPayload())
			}
		}
	}()

	type sample struct {
		time       time.Duration
		memMB      float64
		goroutines float64
	}
	var samples []sample

	start := time.Now()
	sampleTicker := time.NewTicker(sampleInterval)
	defer sampleTicker.Stop()

loop:
	for {
		select {
		case <-sampleTicker.C:
			elapsed := time.Since(start)
			memMB := readServerMemory(cfg.server)
			goroutines := readGoroutines(cfg.server)

			samples = append(samples, sample{
				time:       elapsed,
				memMB:      memMB,
				goroutines: goroutines,
			})
			fmt.Printf("  T+%v: %.0f MB, %.0f goroutines\n", elapsed.Round(time.Second), memMB, goroutines)
		case <-ctx.Done():
			break loop
		}
	}

	pubCancel()
	pubWg.Wait()
	cancel()
	subWg.Wait()

	var memSlope float64
	if len(samples) >= 2 {
		first := samples[0]
		last := samples[len(samples)-1]
		memSlope = (last.memMB - first.memMB) / last.time.Minutes()
	}
	fmt.Printf("  Memory trend: %.1f MB/min\n", memSlope)

	successes, failures, pubLatencies := pub.Stats()
	totalMessages := successes + failures

	finalMem := float64(0)
	if len(samples) > 0 {
		finalMem = samples[len(samples)-1].memMB
	}

	return ScenarioResult{
		Connections:    len(subs),
		PublishQPS:     float64(totalMessages) / totalDuration.Seconds(),
		MemoryMB:       finalMem,
		PublishLatency: ComputeLatencyStats(pubLatencies),
	}
}

// --- scenario D: history replay ---

func runScenarioD(cfg benchConfig) []ScenarioResult {
	fmt.Println("\n=== Scenario D: History Replay ===")

	const channel = "bench.d"
	messageCounts := []int{100, 500, 1000}

	fmt.Printf("  Pre-populating 1000 messages to %s...\n", channel)
	pub := NewPublisher(cfg.server, cfg.apiKey)
	for i := 0; i < 1000; i++ {
		payload, err := json.Marshal(map[string]interface{}{
			"data": fmt.Sprintf("msg-%d", i),
			"pad":  strings.Repeat("x", 900),
		})
		if err != nil {
			fmt.Printf("    marshal pre-populate %d: %v\n", i, err)
			continue
		}
		_, err = pub.Publish(context.Background(), channel, json.RawMessage(payload))
		if err != nil {
			fmt.Printf("    pre-populate %d: %v\n", i, err)
		}
	}
	fmt.Println("  Pre-population complete")

	var results []ScenarioResult

	for _, count := range messageCounts {
		fmt.Printf("\n  Replaying %d messages...\n", count)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		sub := NewSubscriber(cfg.server, cfg.jwtSecret)
		if err := sub.Connect(ctx, "bench-d"); err != nil {
			fmt.Printf("    connect: %v\n", err)
			cancel()
			continue
		}

		start := time.Now()

		if err := sub.Subscribe(ctx, []string{channel}, map[string]int64{channel: 0}); err != nil {
			fmt.Printf("    subscribe: %v\n", err)
			cancel()
			sub.Close()
			continue
		}

		var lastSeq int64
		go func() {
			defer sub.Close()
			defer cancel()
			for {
				msgType, data, err := sub.ReadMessage(ctx)
				if err != nil {
					return
				}
				if msgType != websocket.MessageText {
					continue
				}
				var frame struct {
					Type  string `json:"type"`
					SeqID int64  `json:"seq_id"`
				}
				if err := json.Unmarshal(data, &frame); err != nil {
					continue
				}
				if frame.Type == "message" {
					lastSeq = frame.SeqID
					if lastSeq >= int64(count) {
						return
					}
				}
			}
		}()

		<-ctx.Done()
		sub.Close()
		elapsed := time.Since(start)

		if elapsed.Seconds() == 0 {
			elapsed = time.Millisecond
		}
		replayRate := float64(lastSeq) / elapsed.Seconds()
		fmt.Printf("    Replayed %d messages in %.3f s (%.0f msg/s)\n", lastSeq, elapsed.Seconds(), replayRate)

		results = append(results, ScenarioResult{
			Name:              fmt.Sprintf("D - History Replay (%d msgs)", count),
			HistoryReplaySec:  elapsed.Seconds(),
			HistoryReplayRate: replayRate,
		})
	}

	return results
}

// --- helpers ---

func benchPayload() []byte {
	ts := time.Now().UnixNano()
	// json.Marshal of this shape cannot fail; see BenchmarkMarshalFrame for the
	// serialization path that covers error handling.
	payload, _ := json.Marshal(map[string]interface{}{
		"_bench_ts": ts,
		"data":      strings.Repeat("x", 900),
	})
	return payload
}

func readServerMemory(server string) float64 {
	resp, err := httpGet(server + "/metricsz")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(resp, "\n") {
		if strings.HasPrefix(line, "go_memstats_alloc_bytes ") {
			var val float64
			fmt.Sscanf(line, "go_memstats_alloc_bytes %f", &val)
			return val / (1024 * 1024)
		}
	}
	return 0
}

func readGoroutines(server string) float64 {
	resp, err := httpGet(server + "/metricsz")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(resp, "\n") {
		if strings.HasPrefix(line, "go_goroutines ") {
			var val float64
			fmt.Sscanf(line, "go_goroutines %f", &val)
			return val
		}
	}
	return 0
}

func readCacheHitRate(server string) float64 {
	resp, err := httpGet(server + "/debug/cache")
	if err != nil {
		return 0
	}
	var data struct {
		HitRatePct float64 `json:"hit_rate_pct"`
	}
	if err := json.Unmarshal([]byte(resp), &data); err != nil {
		return 0
	}
	return data.HitRatePct
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}
