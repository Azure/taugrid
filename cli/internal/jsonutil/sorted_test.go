// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonutil

import (
	"fmt"
	"strings"
	"testing"
)

func TestSortedMarshal_NestedMaps(t *testing.T) {
	input := map[string]any{
		"z": "last",
		"a": map[string]any{
			"c": 3,
			"b": 2,
			"a": 1,
		},
		"m": []any{"x", "y"},
	}
	got, err := SortedMarshal(input)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"a":1,"b":2,"c":3},"m":["x","y"],"z":"last"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestSortedMarshal_Deterministic(t *testing.T) {
	input := map[string]any{
		"failure_config": map[string]any{"max_failures": 3},
		"scaling_config": map[string]any{"use_gpu": true, "placement_strategy": "SPREAD"},
		"torch_config":   map[string]any{"timeout_s": 1800, "init_method": "env://"},
	}
	first, err := SortedMarshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		got, err := SortedMarshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("output differs\nfirst: %s\ngot:   %s", first, got)
		}
	}
}

func TestSortedMarshal_Primitives(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "hello", `"hello"`},
		{"int", 42, `42`},
		{"float", 3.14, `3.14`},
		{"bool", true, `true`},
		{"null", nil, `null`},
		{"empty map", map[string]any{}, `{}`},
		{"empty slice", []any{}, `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SortedMarshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSortedMarshal_SizeLimit(t *testing.T) {
	big := map[string]any{}
	for i := range 5000 {
		big["key_"+strings.Repeat("x", 20)+fmt.Sprintf("%d", i)] = strings.Repeat("v", 20)
	}
	_, err := SortedMarshal(big)
	if err == nil {
		t.Fatal("expected size limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}
