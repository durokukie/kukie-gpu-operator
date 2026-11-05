package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	keyPrefix      = "kk-"
	keyRandomBytes = 32
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type keyEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
	Disabled bool   `json:"disabled"`
}

type apiKey struct {
	id       string
	hash     [sha256.Size]byte
	disabled bool
}

// parseKeys validates a key file. Its errors reach the log, so they never carry
// file content: the JSON decoder's messages can quote part of the input.
func parseKeys(data []byte) ([]apiKey, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// A typo such as "disable" must not leave a key the operator meant to revoke working.
	dec.DisallowUnknownFields()
	var file struct {
		Keys []keyEntry `json:"keys"`
	}
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("key file is not valid JSON or has unknown fields")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("key file has data after the JSON document")
	}
	// "keys": [] is a valid way to revoke everything; a file without the field is a mistake.
	if file.Keys == nil {
		return nil, errors.New(`key file has no "keys" list`)
	}

	keys := make([]apiKey, 0, len(file.Keys))
	ids := make(map[string]struct{}, len(file.Keys))
	hashes := make(map[[sha256.Size]byte]struct{}, len(file.Keys))
	for i, entry := range file.Keys {
		if !keyIDPattern.MatchString(entry.ID) || entry.ID == labelUnknown {
			return nil, fmt.Errorf("key entry %d: id is missing or not allowed", i)
		}
		if _, dup := ids[entry.ID]; dup {
			return nil, fmt.Errorf("key entry %d: duplicate id", i)
		}
		ids[entry.ID] = struct{}{}

		raw, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("key entry %d: sha256 must be %d hex characters", i, 2*sha256.Size)
		}
		key := apiKey{id: entry.ID, disabled: entry.Disabled}
		copy(key.hash[:], raw)
		if _, dup := hashes[key.hash]; dup {
			return nil, fmt.Errorf("key entry %d: duplicate sha256", i)
		}
		hashes[key.hash] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

// keyStore holds the current keys and swaps them atomically on reload.
type keyStore struct {
	path         string
	logger       *slog.Logger
	reloadErrors prometheus.Counter

	keys atomic.Pointer[[]apiKey]
	// last is the content of the last good read. Only the reload loop touches it after start.
	last []byte
}

// newKeyStore reads the file once. Failing here is a startup failure.
func newKeyStore(path string, logger *slog.Logger, reloadErrors prometheus.Counter) (*keyStore, error) {
	s := &keyStore{path: path, logger: logger, reloadErrors: reloadErrors}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *keyStore) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if s.keys.Load() != nil && bytes.Equal(data, s.last) {
		return nil
	}
	keys, err := parseKeys(data)
	if err != nil {
		return err
	}
	s.keys.Store(&keys)
	s.last = data
	s.logger.Info("keys loaded", "count", len(keys))
	return nil
}

// watch re-reads the file until ctx ends. It polls rather than uses inotify because
// a mounted Secret is replaced by swapping a symlink.
func (s *keyStore) watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reload(); err != nil {
				s.reloadErrors.Inc()
				s.logger.Error("key file reload failed, keeping the previous keys", "error", err)
			}
		}
	}
}

// lookup finds the key matching presented. It returns the id of a matching key
// (even a disabled one, for metrics) and whether the key may be used.
// Every entry is compared in constant time, with no early exit.
func (s *keyStore) lookup(presented string) (id string, active bool) {
	sum := sha256.Sum256([]byte(presented))
	keys := *s.keys.Load()
	var match *apiKey
	for i := range keys {
		if subtle.ConstantTimeCompare(sum[:], keys[i].hash[:]) == 1 {
			match = &keys[i]
		}
	}
	if match == nil {
		return "", false
	}
	return match.id, !match.disabled
}

// generateKey returns a new key and the entry that stores its hash.
// The id is random rather than derived from the key, so it reveals nothing about it.
func generateKey(name string) (string, keyEntry, error) {
	secret := make([]byte, keyRandomBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", keyEntry{}, err
	}
	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		return "", keyEntry{}, err
	}
	key := keyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(key))
	return key, keyEntry{
		ID:     "key-" + hex.EncodeToString(idBytes),
		Name:   name,
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// runGenkey prints the key on the first line and the key file entry on the second.
// It writes no file: the key exists only in this output.
func runGenkey(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("genkey", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "who the key is for (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *name == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: counter genkey --name <name>")
		return 2
	}
	key, entry, err := generateKey(*name)
	if err != nil {
		fmt.Fprintf(stderr, "generate key: %v\n", err)
		return 1
	}
	line, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(stderr, "encode entry: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n%s\n", key, line)
	fmt.Fprintln(stderr, "line 1 is the API key, shown only once. line 2 goes in the key file's \"keys\" list.")
	return 0
}
