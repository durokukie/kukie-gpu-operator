package main

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
)

const (
	// maxUsageLineBytes bounds one buffered SSE line. A longer line is skipped, not
	// grown into: usage chunks are small, and only those are of interest.
	maxUsageLineBytes = 64 << 10
	// usageTailBytes is how much of a JSON response is kept at least. The buffer never
	// exceeds twice this plus one read. usage sits near the end of the object.
	usageTailBytes = 64 << 10
)

var usageField = []byte(`"usage"`)

// usageCounts uses unsigned fields so a negative count fails to decode: adding a
// negative value to a Prometheus counter panics.
type usageCounts struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
}

type usageEnvelope struct {
	Usage *usageCounts `json:"usage"`
}

type usageMode int

const (
	usageModeNone usageMode = iota
	usageModeSSE
	usageModeJSON
)

// usageMeter finds the usage field in a response as it passes by. It sees the bytes
// on the way to the client, in the same goroutine, so write must stay cheap and never
// block: it only scans and copies into a bounded buffer, and it holds on to no content
// after the response ends. Not safe for concurrent use.
type usageMeter struct {
	mode      usageMode
	buf       []byte
	skipLine  bool // SSE: the current line outgrew the buffer, ignore it up to its newline
	truncated bool // JSON: buf holds only the tail of the body
	usage     *usageCounts
}

func newUsageMeter(header http.Header) *usageMeter {
	m := &usageMeter{}
	if header.Get("Content-Encoding") != "" {
		return m
	}
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return m
	}
	switch mediaType {
	case "text/event-stream":
		m.mode = usageModeSSE
	case "application/json":
		m.mode = usageModeJSON
	}
	return m
}

func (m *usageMeter) write(p []byte) {
	switch m.mode {
	case usageModeSSE:
		m.writeSSE(p)
	case usageModeJSON:
		m.buf = append(m.buf, p...)
		if len(m.buf) > 2*usageTailBytes {
			n := copy(m.buf, m.buf[len(m.buf)-usageTailBytes:])
			m.buf = m.buf[:n]
			m.truncated = true
		}
	}
}

func (m *usageMeter) writeSSE(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.holdPartialLine(p)
			return
		}
		line := p[:i]
		p = p[i+1:]
		switch {
		case m.skipLine:
			m.skipLine = false
		case len(m.buf) == 0:
			m.readSSELine(line)
		default:
			m.holdPartialLine(line)
			if !m.skipLine {
				m.readSSELine(m.buf)
			}
			m.buf = m.buf[:0]
			m.skipLine = false
		}
	}
}

func (m *usageMeter) holdPartialLine(p []byte) {
	if m.skipLine {
		return
	}
	if len(m.buf)+len(p) > maxUsageLineBytes {
		m.buf = m.buf[:0]
		m.skipLine = true
		return
	}
	m.buf = append(m.buf, p...)
}

// readSSELine keeps the last usage seen: with continuous usage stats every chunk
// carries a running total, so summing them would count tokens many times.
func (m *usageMeter) readSSELine(line []byte) {
	payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(payload, usageField) {
		return
	}
	var envelope usageEnvelope
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Usage != nil {
		m.usage = envelope.Usage
	}
}

// finish returns the usage found, or nil. Decoding a JSON body waits until here so it
// cannot delay any byte on its way to the client.
func (m *usageMeter) finish() *usageCounts {
	switch m.mode {
	case usageModeSSE:
		if !m.skipLine && len(m.buf) > 0 {
			m.readSSELine(m.buf)
		}
	case usageModeJSON:
		m.decodeJSON()
	}
	m.buf = nil
	return m.usage
}

func (m *usageMeter) decodeJSON() {
	data := m.buf
	if m.truncated {
		// Quotes inside JSON strings are escaped, so this cannot match generated text.
		// Reopening the object at the field parses only if usage is a top-level field.
		i := bytes.LastIndex(data, usageField)
		if i < 0 {
			return
		}
		data = append([]byte{'{'}, data[i:]...)
	}
	var envelope usageEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil {
		m.usage = envelope.Usage
	}
}
