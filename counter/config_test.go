package main

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"models":{"m":{"upstream":"http://m.ns.svc:8000","maxConcurrent":8}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetryAfterSeconds != defaultRetryAfterSeconds || cfg.MaxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("defaults not applied: %+v", cfg)
	}

	for name, content := range map[string]string{
		"not json":       `nope`,
		"no models":      `{"models":{}}`,
		"typo":           `{"models":{"m":{"upstream":"http://m","maxConcurent":8,"maxConcurrent":1}}}`,
		"trailing":       `{"models":{"m":{"upstream":"http://m","maxConcurrent":1}}} x`,
		"zero limit":     `{"models":{"m":{"upstream":"http://m","maxConcurrent":0}}}`,
		"reserved name":  `{"models":{"unknown":{"upstream":"http://m","maxConcurrent":1}}}`,
		"no scheme":      `{"models":{"m":{"upstream":"m:8000","maxConcurrent":1}}}`,
		"wrong scheme":   `{"models":{"m":{"upstream":"ftp://m","maxConcurrent":1}}}`,
		"query":          `{"models":{"m":{"upstream":"http://m?a=1","maxConcurrent":1}}}`,
		"negative retry": `{"models":{"m":{"upstream":"http://m","maxConcurrent":1}},"retryAfterSeconds":-1}`,
		"negative body":  `{"models":{"m":{"upstream":"http://m","maxConcurrent":1}},"maxBodyBytes":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(content)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	_, err = parseConfig([]byte(`{"models":{"m":{"upstream":"http://user:hunter2@m","maxConcurrent":1}}}`))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("credentials in an upstream must be refused without being echoed: %v", err)
	}
}
