/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// The failure pattern table (DURO-171) turns what a failed Pod shows into a cause, a sentence and a next step for
// status.reason, status.message and status.hint. It is data, not code: the chart installs it as a ConfigMap the manager
// reads as a file, so a new row needs no new image. The table never copies log text into status: a log may hold a token,
// a key or a user's prompt. Only the row's own message goes there, with named groups the row asks for, each checked
// against safeCapture.

//go:embed failure_patterns.yaml
var defaultFailurePatterns []byte

const (
	// ReasonUnclassified is the reason of a Pod failure no row of the table matches.
	ReasonUnclassified = "Unclassified"
	// ReasonStillLoading is the reason of a Loading model whose engine has been starting for longer than
	// Config.StillLoadingAfter. It is not a failure: the phase stays Loading.
	ReasonStillLoading = "StillLoading"

	stageDownload = "download"
	stageEngine   = "engine"

	sourceStatus = "status"
	sourceEvents = "events"
	sourceLog    = "log"

	// failureLogLines is how much of a failed container's log the table sees. vLLM writes the root cause and then the API
	// server's own traceback after it: on a real L40S failure the cause was 64 lines before the end, so 50 lines missed it.
	// The log never goes to status, so reading more exposes nothing.
	failureLogLines = 300
	// failureLogLimitBytes bounds one read. With tailLines the kubelet cuts from the start of the tail, so it has to
	// hold all of failureLogLines or the end, where the cause is, is what gets lost.
	failureLogLimitBytes = 512 * 1024
	maxPatternText  = 512
	maxReasonLength = 64
	maxCapture      = 80
)

// FailurePattern is one row of the table. See failure_patterns.yaml for the meaning of each field.
type FailurePattern struct {
	Reason  string   `json:"reason"`
	Match   string   `json:"match"`
	In      []string `json:"in,omitempty"`
	Stage   string   `json:"stage,omitempty"`
	Message string   `json:"message"`
	Hint    string   `json:"hint"`

	re *regexp.Regexp
}

// FailurePatterns is a parsed table, first row first.
type FailurePatterns struct {
	rows []FailurePattern
}

type failurePatternsFile struct {
	Patterns []FailurePattern `json:"patterns"`
}

var (
	// patternReason fits metav1.Condition's reason pattern as well, so a row's reason could go into a condition.
	patternReason = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	placeholder   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	safeCapture   = regexp.MustCompile(`^[A-Za-z0-9 ._:/,'()\[\]-]+$`)
)

// ParseFailurePatterns reads a table. Every row must be usable: one broken row rejects the whole file, so a typo shows
// in the manager's log at once instead of silently dropping the row it was in.
func ParseFailurePatterns(data []byte) (FailurePatterns, error) {
	var file failurePatternsFile
	if err := yaml.UnmarshalStrict(data, &file); err != nil {
		return FailurePatterns{}, err
	}
	if len(file.Patterns) == 0 {
		return FailurePatterns{}, errors.New("no patterns")
	}
	for i := range file.Patterns {
		row := &file.Patterns[i]
		if err := row.compile(); err != nil {
			return FailurePatterns{}, fmt.Errorf("pattern %d (%s): %w", i+1, row.Reason, err)
		}
	}
	return FailurePatterns{rows: file.Patterns}, nil
}

func (p *FailurePattern) compile() error {
	switch {
	case !patternReason.MatchString(p.Reason) || len(p.Reason) > maxReasonLength:
		return errors.New("reason must be CamelCase starting with an upper-case letter, at most 64 characters")
	case p.Reason == ReasonUnclassified || p.Reason == ReasonStillLoading:
		return fmt.Errorf("reason %s is the oven's own", p.Reason)
	case strings.TrimSpace(p.Message) == "" || strings.TrimSpace(p.Hint) == "":
		return errors.New("message and hint must not be empty")
	case len(p.Message) > maxPatternText || len(p.Hint) > maxPatternText:
		return fmt.Errorf("message and hint must be at most %d bytes", maxPatternText)
	case p.Stage != "" && p.Stage != stageDownload && p.Stage != stageEngine:
		return errors.New("stage must be download, engine or empty")
	}
	for _, in := range p.In {
		if in != sourceStatus && in != sourceEvents && in != sourceLog {
			return fmt.Errorf("in: %q is not status, events or log", in)
		}
	}
	re, err := regexp.Compile(p.Match)
	if err != nil {
		return fmt.Errorf("match: %w", err)
	}
	if p.Match == "" || re.MatchString("") {
		return errors.New("match must not match an empty text")
	}
	for _, m := range placeholder.FindAllStringSubmatch(p.Message, -1) {
		if re.SubexpIndex(m[1]) < 0 {
			return fmt.Errorf("message uses ${%s}, which match does not capture", m[1])
		}
	}
	p.re = re
	return nil
}

// DefaultFailurePatterns is the table built into the manager: the same file the chart installs.
func DefaultFailurePatterns() FailurePatterns {
	p, err := ParseFailurePatterns(defaultFailurePatterns)
	if err != nil {
		panic("built-in failure patterns: " + err.Error())
	}
	return p
}

// failureEvidence is what the table looks at for one failed Pod.
type failureEvidence struct {
	stage string
	// status holds the Kubernetes reasons and messages of the Pod and its containers, exit codes included.
	status string
	// events holds the Pod's (and the model volume's) Warning events as "Reason: message" lines.
	events string
	// log is the end of the failed container's log.
	log string
	// fallback is what an unmatched failure says: the Kubernetes reason and exit code, never the log.
	fallback string
	// logHint tells the user where the raw log is.
	logHint string
}

// diagnosis is what goes into status.
type diagnosis struct {
	reason, message, hint string
}

// classify tries every row on what the Pod says now (status, log) before any row on its events: an event may be older than
// the failure it is read for, so it only decides when nothing current does.
func (p FailurePatterns) classify(ev failureEvidence) diagnosis {
	for _, pass := range [][]struct{ name, text string }{
		{{sourceStatus, ev.status}, {sourceLog, ev.log}},
		{{sourceEvents, ev.events}},
	} {
		for _, row := range p.rows {
			if row.Stage != "" && row.Stage != ev.stage {
				continue
			}
			for _, source := range pass {
				if source.text == "" || (len(row.In) > 0 && !slices.Contains(row.In, source.name)) {
					continue
				}
				if m := row.re.FindStringSubmatch(source.text); m != nil {
					return diagnosis{reason: row.Reason, message: row.expand(m), hint: strings.TrimSpace(row.Hint)}
				}
			}
		}
	}
	return diagnosis{
		reason:  ReasonUnclassified,
		message: strings.TrimSpace("원인을 분류하지 못했다. " + ev.fallback),
		hint:    strings.TrimSpace(ev.logHint + " 같은 원인이 되풀이되면 실패 원인 패턴표에 줄을 더한다(runbooks/oven.md)."),
	}
}

// expand fills the row's ${name} placeholders with what match captured, keeping only short plain values.
func (p FailurePattern) expand(m []string) string {
	return strings.TrimSpace(placeholder.ReplaceAllStringFunc(p.Message, func(ph string) string {
		value := strings.TrimSpace(m[p.re.SubexpIndex(placeholder.FindStringSubmatch(ph)[1])])
		if value == "" || len(value) > maxCapture || !safeCapture.MatchString(value) || redactSecrets(value) != value {
			return "(생략)"
		}
		return value
	}))
}

// FailurePatternSource hands out the table, reading the file again when it changes. The kubelet updates a mounted
// ConfigMap in place, so editing the ConfigMap reaches a running manager within about a minute.
type FailurePatternSource struct {
	// Path is the table file. Empty means the built-in table.
	Path string
	// OnError hears about a file that cannot be read or parsed. The last good table (or the built-in one) stays.
	OnError func(error)

	mu      sync.Mutex
	current *FailurePatterns
	modTime time.Time
	size    int64
}

// Patterns returns the table to use now.
func (s *FailurePatternSource) Patterns() FailurePatterns {
	if s == nil {
		return DefaultFailurePatterns()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		d := DefaultFailurePatterns()
		s.current = &d
	}
	if s.Path == "" {
		return *s.current
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		s.report(err)
		return *s.current
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return *s.current
	}
	data, err := os.ReadFile(s.Path)
	if err == nil {
		var parsed FailurePatterns
		if parsed, err = ParseFailurePatterns(data); err == nil {
			s.current = &parsed
		}
	}
	if err != nil {
		s.report(fmt.Errorf("%s: %w", s.Path, err))
	}
	// A broken file is not read again until it changes.
	s.modTime, s.size = info.ModTime(), info.Size()
	return *s.current
}

func (s *FailurePatternSource) report(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}

// secretShapes are things that must never reach status, even inside a Kubernetes message: Hugging Face, OpenAI-style,
// GitHub and AWS access keys, and "token=…"-style assignments.
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`hf_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b((?:hf_|hugging_face_hub_|huggingface_hub_)?(?:token|api[_-]?key|password|secret|authorization))(["']?\s*[:=]\s*["']?)[^\s"',;]+`),
}

// redactSecrets masks what looks like a credential. Status is read by everyone who can read the ServedModel, and the
// Kukie agent carries it to the web.
func redactSecrets(s string) string {
	for i, re := range secretShapes {
		if i == len(secretShapes)-1 {
			s = re.ReplaceAllString(s, "${1}${2}[가림]")
			continue
		}
		s = re.ReplaceAllString(s, "[가림]")
	}
	return s
}
