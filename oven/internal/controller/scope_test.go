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
	"maps"
	"slices"
	"testing"
)

func TestParseNamespaces(t *testing.T) {
	for in, want := range map[string][]string{
		"":             nil,
		" , ":          nil,
		"llm":          {"llm"},
		"llm, team-a,": {"llm", "team-a"},
		"llm,llm,b":    {"llm", "b"},
	} {
		got, err := ParseNamespaces(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"LLM", "a_b", "a/b", "-a", "a.b"} {
		if _, err := ParseNamespaces(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestCacheOptions(t *testing.T) {
	if got := CacheOptions(nil); got.DefaultNamespaces != nil {
		t.Errorf("no namespaces should watch the whole cluster, got %v", got.DefaultNamespaces)
	}
	got := CacheOptions([]string{"llm", "b"})
	if keys := slices.Sorted(maps.Keys(got.DefaultNamespaces)); !slices.Equal(keys, []string{"b", "llm"}) {
		t.Errorf("DefaultNamespaces = %v", keys)
	}
}
