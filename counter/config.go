package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
)

const (
	defaultRetryAfterSeconds = 2
	defaultMaxBodyBytes      = 16 << 20

	// labelUnknown is the metric label for a model or key the request did not resolve to.
	// Configured names may not use it, so the two meanings cannot mix.
	labelUnknown = "unknown"
)

type config struct {
	Models            map[string]modelConfig `json:"models"`
	RetryAfterSeconds int                    `json:"retryAfterSeconds"`
	MaxBodyBytes      int64                  `json:"maxBodyBytes"`
}

type modelConfig struct {
	Upstream      string `json:"upstream"`
	MaxConcurrent int    `json:"maxConcurrent"`
}

func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(data)
}

func parseConfig(data []byte) (*config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// A misspelled field must not silently fall back to a default limit.
	dec.DisallowUnknownFields()
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse config: unexpected data after the JSON document")
	}

	if len(cfg.Models) == 0 {
		return nil, errors.New("config: models must not be empty")
	}
	for name, m := range cfg.Models {
		if name == "" || name == labelUnknown {
			return nil, fmt.Errorf("config: model name %q is not allowed", name)
		}
		if _, err := parseUpstream(m.Upstream); err != nil {
			return nil, fmt.Errorf("config: model %q: %w", name, err)
		}
		if m.MaxConcurrent < 1 {
			return nil, fmt.Errorf("config: model %q: maxConcurrent must be at least 1", name)
		}
	}

	if cfg.RetryAfterSeconds < 0 || cfg.MaxBodyBytes < 0 {
		return nil, errors.New("config: retryAfterSeconds and maxBodyBytes must not be negative")
	}
	if cfg.RetryAfterSeconds == 0 {
		cfg.RetryAfterSeconds = defaultRetryAfterSeconds
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	return &cfg, nil
}

// parseUpstream validates an upstream base URL. Errors do not echo the value:
// an operator may have put credentials in it.
func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("upstream is not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("upstream must be an http or https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("upstream must not have credentials, a query or a fragment")
	}
	return u, nil
}
