package gateway

import (
	"bytes"
	"encoding/json"
	"io"
)

// Event is one forwarded response's token usage, attributed to the member who
// made it. The gateway carries raw token counts + the model; pricing (weight,
// USD) is applied by the Recorder, so the data plane stays free of the rate
// table. AccountID/PersonID come from the share the key resolved to.
type Event struct {
	AccountID     string
	PersonID      string
	Model         string
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	RequestID     string
	// SessionID is the Claude Code session the request came from, as it names
	// itself (X-Claude-Code-Session-Id); empty for a client that does not say.
	SessionID string
}

// Recorder is where the gateway sends a completed request's usage. It runs off
// the request's hot path (fired when the response body finishes), and must not
// block or error into the response — metering never breaks a forward.
type Recorder interface {
	Record(Event)
}

// respUsage matches Anthropic's `usage` object in both a message_start SSE event
// and a non-streaming JSON body.
type respUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

// usageScanner pulls model + token usage out of a Claude response as its bytes
// flow past, without buffering the whole stream. It handles both shapes:
//
//   - Streaming (SSE): a `message_start` event carries the model plus input and
//     cache tokens; each `message_delta` carries the running output_tokens. We
//     take the model/input from message_start and the last output we see.
//   - Non-streaming (JSON): one object with `model` and `usage`.
//
// It keeps only the current partial line, so memory is bounded regardless of how
// long the stream runs.
type usageScanner struct {
	line  []byte
	seen  bool
	model string
	u     respUsage
}

// maxLine caps the partial-line buffer so a pathological newline-free stream
// can't grow it without bound; a real message_start line or JSON body is far
// smaller than this.
const maxLine = 1 << 20

func (s *usageScanner) feed(p []byte) {
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			break
		}
		s.processLine(s.line[:i])
		s.line = s.line[i+1:]
	}
	if len(s.line) > maxLine {
		s.line = s.line[len(s.line)-maxLine:]
	}
}

// finish processes any trailing partial line (a JSON body with no final newline)
// and returns whether usage was found.
func (s *usageScanner) finish() bool {
	if len(s.line) > 0 {
		s.processLine(s.line)
		s.line = nil
	}
	return s.seen
}

func (s *usageScanner) processLine(line []byte) {
	data := bytes.TrimSpace(line)
	if bytes.HasPrefix(data, []byte("data:")) {
		data = bytes.TrimSpace(data[len("data:"):])
	}
	if len(data) == 0 || data[0] != '{' {
		return
	}
	switch {
	case bytes.Contains(data, []byte(`"message_start"`)):
		var ev struct {
			Message struct {
				Model string    `json:"model"`
				Usage respUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Message.Model != "" {
			s.seen = true
			s.model = ev.Message.Model
			s.u.Input = ev.Message.Usage.Input
			s.u.CacheCreation = ev.Message.Usage.CacheCreation
			s.u.CacheRead = ev.Message.Usage.CacheRead
			if ev.Message.Usage.Output > 0 {
				s.u.Output = ev.Message.Usage.Output
			}
		}
	case bytes.Contains(data, []byte(`"message_delta"`)):
		var ev struct {
			Usage respUsage `json:"usage"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Usage.Output > 0 {
			s.u.Output = ev.Usage.Output // cumulative, last one wins
		}
	case bytes.Contains(data, []byte(`"usage"`)) && bytes.Contains(data, []byte(`"model"`)):
		// A non-streaming full response body.
		var ev struct {
			Model string    `json:"model"`
			Usage respUsage `json:"usage"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Model != "" {
			s.seen = true
			s.model = ev.Model
			s.u = ev.Usage
		}
	}
}

// meteringBody wraps a response body: bytes pass through untouched (so streaming
// keeps streaming) while a copy feeds the scanner. When the body is fully read
// or closed, it fires the recorder once with whatever usage was found.
type meteringBody struct {
	inner io.ReadCloser
	scan  usageScanner
	rec   Recorder
	base  Event // AccountID/PersonID/RequestID, filled before usage
	done  bool
}

func (b *meteringBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.scan.feed(p[:n])
	}
	if err == io.EOF {
		b.emit()
	}
	return n, err
}

func (b *meteringBody) Close() error {
	b.emit()
	return b.inner.Close()
}

func (b *meteringBody) emit() {
	if b.done {
		return
	}
	b.done = true
	if !b.scan.finish() || b.rec == nil {
		return
	}
	ev := b.base
	ev.Model = b.scan.model
	ev.Input = b.scan.u.Input
	ev.Output = b.scan.u.Output
	ev.CacheCreation = b.scan.u.CacheCreation
	ev.CacheRead = b.scan.u.CacheRead
	b.rec.Record(ev)
}
