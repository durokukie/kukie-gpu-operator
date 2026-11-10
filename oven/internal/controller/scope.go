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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ParseNamespaces turns "a,b" into the namespaces the oven watches. Empty input gives nil, which means every namespace.
func ParseNamespaces(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for name := range strings.SplitSeq(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("%q is not a namespace name: %s", name, strings.Join(errs, "; "))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// CacheOptions limits the informers to the given namespaces, so list and watch go to those namespaces only and a Role in
// each is enough. No namespaces means the whole cluster, which needs those verbs in a ClusterRole.
func CacheOptions(namespaces []string) cache.Options {
	if len(namespaces) == 0 {
		return cache.Options{}
	}
	defaults := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		defaults[ns] = cache.Config{}
	}
	return cache.Options{DefaultNamespaces: defaults}
}

// ClientOptions keeps ConfigMaps and Nodes out of the cache. A few reads per finished measurement: cached, the ConfigMaps
// of every watched namespace would sit in the oven's memory. Nodes likewise: one read per finished measurement and per
// look for a baseline to reuse, and caching them would need list and watch on every node (the ClusterRole has get only).
func ClientOptions() client.Options {
	return client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Node{}}}}
}
