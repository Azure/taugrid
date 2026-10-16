// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var managedPassthroughPaths = map[string]bool{
	"eval":                        false,
	"artifacts":                   false,
	"research":                    false,
	"model":                       false,
	"resource_naming":             false,
	"compute.cpus":                false,
	"compute.cpu_limit":           false,
	"compute.memory":              false,
	"compute.memory_limit":        false,
	"compute.worker_cpus":         false,
	"compute.worker_cpu_limit":    false,
	"compute.worker_memory":       false,
	"compute.worker_memory_limit": false,
	"runtime.rdma":                true,
	"runtime.rdma.enabled":        false,
	"storage.mounts":              true,
	"storage.mounts.name":         false,
	"storage.mounts.mountPath":    false,
	"storage.mounts.pvc":          false,
	"storage.mounts.readOnly":     false,
}

func configSchemaPaths() map[string]bool {
	out := map[string]bool{}
	for _, path := range configFieldPaths() {
		out[path] = false
	}
	for _, path := range configStructPaths() {
		out[path] = true
	}
	return out
}

func ManagedPassthroughPaths() []string {
	out := make([]string, 0, len(managedPassthroughPaths))
	for path := range managedPassthroughPaths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func IsKnownManagedKey(path string) bool {
	if _, ok := managedPassthroughPaths[path]; ok {
		return true
	}
	for prefix := path; ; {
		idx := strings.LastIndex(prefix, ".")
		if idx < 0 {
			break
		}
		prefix = prefix[:idx]
		if descend, ok := managedPassthroughPaths[prefix]; ok {
			return !descend
		}
		if descend, ok := configSchemaPaths()[prefix]; ok && !descend {
			return true
		}
	}
	_, ok := configSchemaPaths()[path]
	return ok
}

type UnknownKey struct {
	Path     string
	Children []string
}

func UnknownKeys(raw []byte) ([]UnknownKey, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	known := configSchemaPaths()
	var unknown []UnknownKey
	collectUnknownNode(doc.Content[0], "", known, &unknown)
	unknown = dedupeUnknown(unknown)
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Path < unknown[j].Path })
	return unknown, nil
}

func dedupeUnknown(in []UnknownKey) []UnknownKey {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, key := range in {
		if seen[key.Path] {
			continue
		}
		seen[key.Path] = true
		out = append(out, key)
	}
	return out
}

func collectUnknownNode(node *yaml.Node, prefix string, known map[string]bool, out *[]UnknownKey) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		collectUnknown(node, prefix, known, out)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			collectUnknownNode(item, prefix, known, out)
		}
	}
}

func collectUnknown(node *yaml.Node, prefix string, known map[string]bool, out *[]UnknownKey) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if descend, ok := managedPassthroughPaths[path]; ok {
			if descend {
				collectUnknownNode(node.Content[i+1], path, known, out)
			}
			continue
		}
		descend, ok := known[path]
		if !ok {
			*out = append(*out, UnknownKey{Path: path, Children: childKeys(node.Content[i+1])})
			continue
		}
		if descend {
			collectUnknownNode(node.Content[i+1], path, known, out)
		}
	}
}

func childKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func describeUnknownKey(key UnknownKey, source string) string {
	msg := fmt.Sprintf("warning: unknown config key %q in %s (ignored)", key.Path, source)
	if suggestion, ok := suggestForKey(key); ok {
		msg += fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return msg
}

func suggestForKey(key UnknownKey) (string, bool) {
	if suggestion, ok := suggestFieldPath(key.Path); ok {
		return suggestion, true
	}
	return suggestParentFromChildren(key)
}

func suggestParentFromChildren(key UnknownKey) (string, bool) {
	if len(key.Children) == 0 {
		return "", false
	}
	parents := map[string]int{}
	for _, child := range key.Children {
		suggestion, ok := suggestFieldPath(key.Path + "." + child)
		if !ok {
			continue
		}
		idx := strings.LastIndex(suggestion, ".")
		if idx < 0 {
			continue
		}
		parents[suggestion[:idx]]++
	}
	if len(parents) != 1 {
		return "", false
	}
	for parent, count := range parents {
		if count == len(key.Children) {
			return parent, true
		}
	}
	return "", false
}

func suggestFieldPath(path string) (string, bool) {
	candidates := configFieldPaths()
	leaf, depth := leafAndDepth(path)

	best := ""
	bestDistance := -1
	bestPathDistance := -1
	for _, candidate := range candidates {
		candidateLeaf, candidateDepth := leafAndDepth(candidate)
		pathDistance := levenshtein(path, candidate)
		distance := pathDistance
		if candidateDepth == depth {
			if leafDistance := levenshtein(leaf, candidateLeaf); leafDistance < distance {
				distance = leafDistance
			}
		}
		if bestDistance < 0 || distance < bestDistance ||
			(distance == bestDistance && pathDistance < bestPathDistance) {
			best, bestDistance, bestPathDistance = candidate, distance, pathDistance
		}
	}
	if bestDistance < 0 || bestDistance > maxSuggestionDistance(leaf) {
		return "", false
	}
	return best, true
}

func leafAndDepth(path string) (string, int) {
	depth := strings.Count(path, ".")
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		return path[idx+1:], depth
	}
	return path, depth
}

func maxSuggestionDistance(leaf string) int {
	limit := len(leaf) / 3
	if limit < 2 {
		limit = 2
	}
	if limit > 6 {
		limit = 6
	}
	return limit
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
