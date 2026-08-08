// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonutil

import (
	"encoding/json"
	"fmt"
	"sort"
)

const maxJSONSize = 64 * 1024 // 64 KiB

// SortedMarshal serializes v to JSON with map keys sorted alphabetically at
// every nesting level. This guarantees deterministic output: identical input
// always produces byte-identical JSON, regardless of Go map iteration order.
func SortedMarshal(v any) ([]byte, error) {
	sorted := sortKeys(v)
	b, err := json.Marshal(sorted)
	if err != nil {
		return nil, err
	}
	if len(b) > maxJSONSize {
		return nil, fmt.Errorf("serialized JSON exceeds %d KiB limit (%d bytes)", maxJSONSize/1024, len(b))
	}
	return b, nil
}

func sortKeys(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make([]orderedEntry, len(keys))
		for i, k := range keys {
			ordered[i] = orderedEntry{Key: k, Value: sortKeys(val[k])}
		}
		return orderedMap(ordered)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = sortKeys(item)
		}
		return out
	default:
		return v
	}
}

type orderedEntry struct {
	Key   string
	Value any
}

type orderedMap []orderedEntry

func (o orderedMap) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, entry := range o {
		if i > 0 {
			buf = append(buf, ',')
		}
		key, err := json.Marshal(entry.Key)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(entry.Value)
		if err != nil {
			return nil, err
		}
		buf = append(buf, key...)
		buf = append(buf, ':')
		buf = append(buf, val...)
	}
	buf = append(buf, '}')
	return buf, nil
}
