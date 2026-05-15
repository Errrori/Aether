package hub

import (
	"context"
	"sync"

	"github.com/aether-mq/aether/internal/auth"
)

// Connection represents a subscriber connection managed by the Hub.
// It is transport-agnostic: the ws layer creates it, writes bytes to Send,
// and sets Overflow to handle backpressure.
type Connection struct {
	ID           string
	SubscriberID string
	Claims       *auth.Claims

	// Send is the buffered outbound channel. Hub pushes JSON frame bytes here.
	Send chan []byte

	ctx    context.Context
	cancel context.CancelFunc

	channels map[string]struct{}
	cursors  map[string]int64
	mu       sync.Mutex

	// Overflow is called when the Send buffer is full. The ws layer sets this
	// to send WebSocket close frame 1012 and terminate the connection.
	Overflow func()

	closeOnce sync.Once
}

// NewConnection creates a new Connection with a buffered Send channel.
func NewConnection(id, subscriberID string, claims *auth.Claims, bufferSize int) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	return &Connection{
		ID:           id,
		SubscriberID: subscriberID,
		Claims:       claims,
		Send:         make(chan []byte, bufferSize),
		ctx:          ctx,
		cancel:       cancel,
		channels:     make(map[string]struct{}),
		cursors:      make(map[string]int64),
	}
}

// AddChannel adds a channel to the connection's subscription set.
// Returns false if the channel was already subscribed.
func (c *Connection) AddChannel(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.channels[channel]; ok {
		return false
	}
	c.channels[channel] = struct{}{}
	return true
}

// RemoveChannel removes a channel from the connection's subscription set.
func (c *Connection) RemoveChannel(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.channels, channel)
	delete(c.cursors, channel)
}

// HasChannel reports whether the connection is subscribed to the given channel.
func (c *Connection) HasChannel(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.channels[channel]
	return ok
}

// ChannelCount returns the number of channels the connection is subscribed to.
func (c *Connection) ChannelCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.channels)
}

// Channels returns a snapshot of subscribed channel names.
func (c *Connection) Channels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, 0, len(c.channels))
	for ch := range c.channels {
		result = append(result, ch)
	}
	return result
}

// GetCursor returns the last delivered seq_id for a channel.
func (c *Connection) GetCursor(channel string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq, ok := c.cursors[channel]
	return seq, ok
}

// SetCursor records the last delivered seq_id for a channel.
func (c *Connection) SetCursor(channel string, seqID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursors[channel] = seqID
}

// Close shuts down the connection: calls Overflow (if set) and cancels the context.
// Safe to call multiple times.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		if c.Overflow != nil {
			c.Overflow()
		}
		c.cancel()
	})
}

// Done returns a channel that is closed when the connection is shut down.
func (c *Connection) Done() <-chan struct{} {
	return c.ctx.Done()
}
