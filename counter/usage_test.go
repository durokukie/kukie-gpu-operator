package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func sseMeter() *usageMeter {
	return newUsageMeter(http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}})
}

func jsonMeter() *usageMeter {
	return newUsageMeter(http.Header{"Content-Type": {"application/json"}})
}

func requireUsage(t *testing.T, got *usageCounts, prompt, completion uint64) {
	t.Helper()
	if got == nil {
		t.Fatal("no usage found")
	}
	if got.PromptTokens != prompt || got.CompletionTokens != completion {
		t.Fatalf("usage = %+v, want %d/%d", *got, prompt, completion)
	}
}

const sseUsage = `data: {"usage":{"prompt_tokens":1,"completion_tokens":2}}`

func TestUsageSSE(t *testing.T) {
	t.Run("split at every byte", func(t *testing.T) {
		m := sseMeter()
		for _, b := range []byte("data: {\"n\":1}\n\n" + sseUsage + "\n\ndata: [DONE]\n\n") {
			m.write([]byte{b})
		}
		requireUsage(t, m.finish(), 1, 2)
	})
	t.Run("crlf and no space after the colon", func(t *testing.T) {
		m := sseMeter()
		m.write([]byte(`data:{"usage":{"prompt_tokens":5,"completion_tokens":6}}` + "\r\n\r\n"))
		requireUsage(t, m.finish(), 5, 6)
	})
	t.Run("last line without newline", func(t *testing.T) {
		m := sseMeter()
		m.write([]byte(sseUsage))
		requireUsage(t, m.finish(), 1, 2)
	})
	t.Run("running totals are not summed", func(t *testing.T) {
		m := sseMeter()
		m.write([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":1}}` + "\n\n"))
		m.write([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\n"))
		m.write([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n"))
		requireUsage(t, m.finish(), 10, 3)
	})
	t.Run("null usage does not erase a real one", func(t *testing.T) {
		m := sseMeter()
		m.write([]byte(sseUsage + "\n\ndata: {\"usage\":null}\n\n"))
		requireUsage(t, m.finish(), 1, 2)
	})
	for name, stream := range map[string]string{
		"no usage":          "data: {\"n\":1}\n\ndata: [DONE]\n\n",
		"null only":         "data: {\"usage\":null}\n\n",
		"word in generated": `data: {"content":"\"usage\":{\"prompt_tokens\":9}"}` + "\n\n",
		"not a data line":   `event: {"usage":{"prompt_tokens":9}}` + "\n\n",
		"negative count":    `data: {"usage":{"prompt_tokens":-1,"completion_tokens":2}}` + "\n\n",
		"cut off":           `data: {"usage":{"prompt_tokens":1,"comple`,
	} {
		t.Run(name, func(t *testing.T) {
			m := sseMeter()
			m.write([]byte(stream))
			if got := m.finish(); got != nil {
				t.Fatalf("usage = %+v, want none", *got)
			}
		})
	}
}

func TestUsageSSELongLineDoesNotGrowMemory(t *testing.T) {
	m := sseMeter()
	chunk := []byte(strings.Repeat("x", 32<<10))
	m.write([]byte(`data: {"usage":{"prompt_tokens":9,"completion_tokens":9},"pad":"`))
	for range 1024 { // 32 MiB without a newline.
		m.write(chunk)
		if cap(m.buf) > 2*maxUsageLineBytes {
			t.Fatalf("buffer grew to %d bytes", cap(m.buf))
		}
	}
	m.write([]byte("\"}\n\n"))
	if m.skipLine || len(m.buf) != 0 {
		t.Fatalf("meter did not recover: skip %v, %d bytes held", m.skipLine, len(m.buf))
	}
	// The oversized line is ignored and the next one is still read.
	m.write([]byte(sseUsage + "\n\n"))
	requireUsage(t, m.finish(), 1, 2)
}

func TestUsageJSON(t *testing.T) {
	t.Run("small body", func(t *testing.T) {
		m := jsonMeter()
		m.write([]byte(`{"id":"x","usage":{"prompt_tokens":`))
		m.write([]byte(`3,"completion_tokens":4}}`))
		requireUsage(t, m.finish(), 3, 4)
	})
	t.Run("no usage", func(t *testing.T) {
		m := jsonMeter()
		m.write([]byte(`{"id":"x"}`))
		if got := m.finish(); got != nil {
			t.Fatalf("usage = %+v", *got)
		}
	})
	t.Run("large body keeps only the tail", func(t *testing.T) {
		m := jsonMeter()
		m.write([]byte(`{"choices":[{"text":"`))
		chunk := []byte(strings.Repeat("x", 32<<10))
		for range 1024 {
			m.write(chunk)
			if len(m.buf) > 2*usageTailBytes {
				t.Fatalf("buffer holds %d bytes", len(m.buf))
			}
		}
		m.write([]byte(`"}],"usage":{"prompt_tokens":7,"completion_tokens":8},"prompt_logprobs":null}`))
		requireUsage(t, m.finish(), 7, 8)
	})
	t.Run("large body with a nested usage only", func(t *testing.T) {
		m := jsonMeter()
		m.write([]byte(strings.Repeat("x", 3*usageTailBytes)))
		m.write([]byte(`{"nested":{"usage":{"prompt_tokens":7}}}`))
		if got := m.finish(); got != nil {
			t.Fatalf("usage = %+v", *got)
		}
	})
}

func TestUsageNotReadFromOtherResponses(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	for name, header := range map[string]http.Header{
		"gzip":         {"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		"plain text":   {"Content-Type": {"text/plain"}},
		"no type":      {},
		"invalid type": {"Content-Type": {";;;"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newUsageMeter(header)
			m.write(body)
			if got := m.finish(); got != nil {
				t.Fatalf("usage = %+v", *got)
			}
			if m.buf != nil {
				t.Fatalf("meter holds %d bytes", len(m.buf))
			}
		})
	}
}

func BenchmarkUsageSSEChunk(b *testing.B) {
	m := sseMeter()
	chunk := fmt.Appendf(nil, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", strings.Repeat("x", 32))
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		m.write(chunk)
	}
}
