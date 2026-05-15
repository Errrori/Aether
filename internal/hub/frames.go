package hub

import "encoding/json"

// Frame type constants.
const (
	FrameTypeMessage      = "message"
	FrameTypeSubscribed   = "subscribed"
	FrameTypeUnsubscribed = "unsubscribed"
	FrameTypeGap          = "gap"
	FrameTypeError        = "error"
)

// Error codes sent to clients.
const (
	ErrCodeInvalidChannel = 40001
	ErrCodeUnauthorized   = 40301
	ErrCodeTooManyPerReq  = 40005
	ErrCodeTooManyPerConn = 40006
	ErrCodeHistoryFailed  = 50001
)

// MessageFrame pushes a real-time message to a subscriber.
type MessageFrame struct {
	Type      string          `json:"type"`
	Channel   string          `json:"channel"`
	SeqID     int64           `json:"seq_id"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// SubscribedFrame confirms channels were successfully subscribed.
type SubscribedFrame struct {
	Type     string   `json:"type"`
	Channels []string `json:"channels"`
}

// UnsubscribedFrame confirms channels were unsubscribed.
type UnsubscribedFrame struct {
	Type     string   `json:"type"`
	Channels []string `json:"channels"`
}

// GapFrame indicates a message history gap.
type GapFrame struct {
	Type             string `json:"type"`
	Channel          string `json:"channel"`
	AvailableFromSeq int64  `json:"available_from_seq"`
	RequestedFromSeq int64  `json:"requested_from_seq"`
	Message          string `json:"message"`
}

// ErrorFrame conveys a protocol error to the client.
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MarshalFrame serializes a frame value to JSON with a newline terminator.
func MarshalFrame(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	return data, nil
}
