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

package main

import (
	"flag"
	"io"
	"maps"
	"testing"
	"time"

	"github.com/durokukie/kukie-gpu-operator/oven/internal/controller"
)

func parseConfig(t *testing.T, args ...string) (controller.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	finish := bindConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return finish()
}

func TestConfigDefaults(t *testing.T) {
	c, err := parseConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRuntime != 2*time.Hour {
		t.Errorf("MaxRuntime = %v, want 2h", c.MaxRuntime)
	}
	if c.NodeWaitTimeout != 15*time.Minute {
		t.Errorf("NodeWaitTimeout = %v, want 15m", c.NodeWaitTimeout)
	}
	if c.DownloadDeadline != controller.DefaultDownloadDeadline {
		t.Errorf("DownloadDeadline = %v", c.DownloadDeadline)
	}
	if c.Measure.Deadline != controller.DefaultMeasureDeadline {
		t.Errorf("Measure.Deadline = %v", c.Measure.Deadline)
	}
}

// Every flag the chart renders has to reach the Config the reconciler gets.
func TestConfigFlagsReachTheConfig(t *testing.T) {
	c, err := parseConfig(t,
		"--download-image=hf:1", "--gpu-node-selector=a=b,c=d", "--download-deadline=3h",
		"--max-runtime=4h", "--node-wait-timeout=30m",
		"--measure-deadline=30m", "--measure-concurrency=1 2", "--measure-input-tokens=10", "--measure-output-tokens=20",
		"--measure-ttft-ms=30", "--measure-itl-ms=40",
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.DownloadImage != "hf:1" || c.DownloadDeadline != 3*time.Hour {
		t.Errorf("download flags: %+v", c)
	}
	if !maps.Equal(c.GPUNodeSelector, map[string]string{"a": "b", "c": "d"}) {
		t.Errorf("GPUNodeSelector = %v", c.GPUNodeSelector)
	}
	if c.MaxRuntime != 4*time.Hour {
		t.Errorf("MaxRuntime = %v, want 4h", c.MaxRuntime)
	}
	if c.NodeWaitTimeout != 30*time.Minute {
		t.Errorf("NodeWaitTimeout = %v, want 30m", c.NodeWaitTimeout)
	}
	want := controller.MeasureConfig{Deadline: 30 * time.Minute, Concurrency: "1 2", InputLen: 10, OutputLen: 20, TTFTMillis: 30, ITLMillis: 40}
	if c.Measure != want {
		t.Errorf("Measure = %+v, want %+v", c.Measure, want)
	}
}

func TestZeroTurnsTheLimitsOff(t *testing.T) {
	c, err := parseConfig(t, "--max-runtime=0", "--node-wait-timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRuntime != 0 || c.NodeWaitTimeout != 0 {
		t.Errorf("got %v and %v, want both 0", c.MaxRuntime, c.NodeWaitTimeout)
	}
}

func TestBadConfigIsRefused(t *testing.T) {
	for _, args := range [][]string{{"--max-runtime=-1h"}, {"--node-wait-timeout=-1s"}, {"--gpu-node-selector=novalue"}} {
		if _, err := parseConfig(t, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}
