package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Publisher sends messages to Aether via HTTP publish API.
type Publisher struct {
	serverURL string
	apiKey    string
	client    *http.Client

	mu       sync.Mutex
	latencies []time.Duration
	successes int64
	failures  int64
}

// NewPublisher creates a publisher for the given server and API key.
func NewPublisher(serverURL, apiKey string) *Publisher {
	return &Publisher{
		serverURL: strings.TrimRight(serverURL, "/"),
		apiKey:    apiKey,
		client: &http.Client{
			Transport: &http.Transport{
				MaxConnsPerHost: 100,
			},
			Timeout: 10 * time.Second,
		},
	}
}

type publishBody struct {
	Channel string          `json:"channel"`
	Payload json.RawMessage `json:"payload"`
}

type publishResponse struct {
	OK        bool   `json:"ok"`
	SeqID     int64  `json:"seq_id"`
	Timestamp string `json:"timestamp"`
}

// Publish sends a single message. Returns publish duration (HTTP round-trip).
func (p *Publisher) Publish(ctx context.Context, channel string, payload json.RawMessage) (time.Duration, error) {
	body, err := json.Marshal(publishBody{Channel: channel, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("marshal publish body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.serverURL+"/api/v1/publish", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.client.Do(req)
	d := time.Since(start)
	if err != nil {
		p.recordFailure()
		return d, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.recordFailure()
		return d, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		p.recordFailure()
		return d, fmt.Errorf("publish returned %d: %s", resp.StatusCode, string(respBody))
	}

	var pr publishResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		p.recordFailure()
		return d, fmt.Errorf("unmarshal response: %w", err)
	}
	if !pr.OK {
		p.recordFailure()
		return d, fmt.Errorf("publish not ok")
	}

	p.recordSuccess(d)
	return d, nil
}

func (p *Publisher) recordSuccess(d time.Duration) {
	p.mu.Lock()
	p.successes++
	p.latencies = append(p.latencies, d)
	p.mu.Unlock()
}

func (p *Publisher) recordFailure() {
	p.mu.Lock()
	p.failures++
	p.mu.Unlock()
}

// Stats returns aggregate publish statistics.
func (p *Publisher) Stats() (successes, failures int64, latencies []time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.successes, p.failures, append([]time.Duration(nil), p.latencies...)
}
