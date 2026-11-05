package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseKeysRejectsBadFiles(t *testing.T) {
	good := hashOf("a")
	for name, content := range map[string]string{
		"not json":       `nope`,
		"empty object":   `{}`,
		"null":           `null`,
		"unknown field":  `{"keys":[{"id":"a","sha256":"` + good + `","disable":true}]}`,
		"trailing":       `{"keys":[]} x`,
		"no id":          `{"keys":[{"sha256":"` + good + `"}]}`,
		"reserved id":    `{"keys":[{"id":"unknown","sha256":"` + good + `"}]}`,
		"short hash":     `{"keys":[{"id":"a","sha256":"abcd"}]}`,
		"not hex":        `{"keys":[{"id":"a","sha256":"` + strings.Repeat("z", 64) + `"}]}`,
		"duplicate id":   `{"keys":[{"id":"a","sha256":"` + good + `"},{"id":"a","sha256":"` + hashOf("b") + `"}]}`,
		"duplicate hash": `{"keys":[{"id":"a","sha256":"` + good + `"},{"id":"b","sha256":"` + good + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseKeys([]byte(content)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	keys, err := parseKeys([]byte(`{"keys":[]}`))
	if err != nil || len(keys) != 0 {
		t.Fatalf("an empty list revokes everything and is valid: %v %v", keys, err)
	}
}

func TestParseKeysErrorsHoldNoFileContent(t *testing.T) {
	const secret = "kk-raw-key-pasted-by-mistake"
	for _, content := range []string{
		secret,
		`{"keys":[{"id":"a","sha256":"` + secret + `"}]}`,
		`{"` + secret + `":1}`,
		`{"keys":[{"id":"` + secret + `!","sha256":"x"}]}`,
	} {
		_, err := parseKeys([]byte(content))
		if err == nil {
			t.Fatalf("expected an error for %q", content)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "kk-") {
			t.Fatalf("error quotes the file: %v", err)
		}
	}
}

func testStore(t *testing.T, content string) (*keyStore, string, prometheus.Counter, *syncBuffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	writeFile(t, path, content)
	logs := &syncBuffer{}
	errs := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_errors"})
	s, err := newKeyStore(path, slog.New(slog.NewJSONHandler(logs, nil)), errs)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, errs, logs
}

func requireLookup(t *testing.T, s *keyStore, key, wantID string, wantActive bool) {
	t.Helper()
	id, active := s.lookup(key)
	if id != wantID || active != wantActive {
		t.Fatalf("lookup = (%q, %v), want (%q, %v)", id, active, wantID, wantActive)
	}
}

func TestKeyStartupFailure(t *testing.T) {
	discard := slog.New(slog.NewJSONHandler(io.Discard, nil))
	errs := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_errors"})
	if _, err := newKeyStore(filepath.Join(t.TempDir(), "missing"), discard, errs); err == nil {
		t.Fatal("a missing key file must fail startup")
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	writeFile(t, path, "{broken")
	if _, err := newKeyStore(path, discard, errs); err == nil {
		t.Fatal("a broken key file must fail startup")
	}
}

func TestKeyReloadAppliesAddAndRevoke(t *testing.T) {
	s, path, _, _ := testStore(t, string(keyFile(t, keyEntry{ID: "a", SHA256: hashOf("key-a")})))
	requireLookup(t, s, "key-a", "a", true)
	requireLookup(t, s, "key-b", "", false)

	writeFile(t, path, string(keyFile(t,
		keyEntry{ID: "a", SHA256: hashOf("key-a"), Disabled: true},
		keyEntry{ID: "b", SHA256: hashOf("key-b")},
	)))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	requireLookup(t, s, "key-a", "a", false)
	requireLookup(t, s, "key-b", "b", true)
}

func TestWatchKeepsPreviousKeysOnBadFileAndRecovers(t *testing.T) {
	s, path, errs, logs := testStore(t, string(keyFile(t, keyEntry{ID: "a", SHA256: hashOf("key-a")})))
	ctx := t.Context()
	go s.watch(ctx, 5*time.Millisecond)

	counterValue := func() float64 {
		var out dto.Metric
		ch := make(chan prometheus.Metric, 1)
		errs.Collect(ch)
		if err := (<-ch).Write(&out); err != nil {
			t.Fatal(err)
		}
		return out.GetCounter().GetValue()
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(waitTimeout)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	writeFile(t, path, "{broken "+hashOf("key-a"))
	eventually("a reload error", func() bool { return counterValue() >= 1 })
	requireLookup(t, s, "key-a", "a", true)
	if !strings.Contains(logs.String(), "keeping the previous keys") {
		t.Fatalf("no error log: %s", logs.String())
	}
	if strings.Contains(logs.String(), hashOf("key-a")) {
		t.Fatal("log holds a hash")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before := counterValue()
	eventually("a read error", func() bool { return counterValue() > before })
	requireLookup(t, s, "key-a", "a", true)

	writeFile(t, path, string(keyFile(t, keyEntry{ID: "b", SHA256: hashOf("key-b")})))
	eventually("recovery", func() bool { id, _ := s.lookup("key-b"); return id == "b" })
	requireLookup(t, s, "key-a", "", false)
}

func TestGenkeyOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"genkey", "--name", "alice"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want 2", len(lines))
	}
	key := lines[0]
	encoded, ok := strings.CutPrefix(key, keyPrefix)
	if !ok {
		t.Fatalf("key has no prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != keyRandomBytes {
		t.Fatalf("key body decodes to %d bytes, err %v", len(raw), err)
	}

	var entry keyEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if entry.Name != "alice" || entry.Disabled || entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("entry = %+v", entry)
	}
	if strings.Contains(lines[1], encoded) || strings.Contains(stderr.String(), encoded) {
		t.Fatal("the key is in the entry line or on stderr")
	}

	// The printed entry goes into a key file as is, and the printed key then works.
	path := filepath.Join(t.TempDir(), "keys.json")
	writeFile(t, path, `{"keys":[`+lines[1]+`]}`)
	s, err := newKeyStore(path, slog.New(slog.NewJSONHandler(io.Discard, nil)),
		prometheus.NewCounter(prometheus.CounterOpts{Name: "test_errors"}))
	if err != nil {
		t.Fatal(err)
	}
	requireLookup(t, s, key, entry.ID, true)

	entries, err := os.ReadDir(t.TempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
}

func TestGenkeyNeedsName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"genkey"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
