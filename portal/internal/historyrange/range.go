// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package historyrange validates temporal query parameters shared by Portal APIs.
package historyrange

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const MaxWindow = 30 * 24 * time.Hour

// Range is a validated historical query. Since is retained for legacy Stellar
// callers; Window and absolute Start/End are mutually exclusive.
type Range struct {
	Window time.Duration
	Start  time.Time
	End    time.Time
	Since  string
}

// Parse validates a historical range and rejects repeated or mixed parameters.
func Parse(q url.Values, allowSince bool) (Range, error) {
	window, hasWindow, err := singleValue(q, "window")
	if err != nil {
		return Range{}, err
	}
	start, hasStart, err := singleValue(q, "start")
	if err != nil {
		return Range{}, err
	}
	end, hasEnd, err := singleValue(q, "end")
	if err != nil {
		return Range{}, err
	}
	since, hasSince, err := singleValue(q, "since")
	if err != nil {
		return Range{}, err
	}
	if hasSince && !allowSince {
		return Range{}, fmt.Errorf("since is not supported for this endpoint")
	}
	if hasSince && (hasWindow || hasStart || hasEnd) {
		return Range{}, fmt.Errorf("since cannot be combined with window/start/end")
	}
	if hasWindow && (hasStart || hasEnd) {
		return Range{}, fmt.Errorf("use either window or start/end, not both")
	}
	if hasSince {
		if since == "" {
			return Range{}, fmt.Errorf("since must not be empty")
		}
		return Range{Since: since}, nil
	}
	if hasWindow {
		windowDuration, parseErr := time.ParseDuration(window)
		if parseErr != nil || windowDuration <= 0 || windowDuration > MaxWindow {
			return Range{}, fmt.Errorf("window must be a positive duration no greater than %s", MaxWindow)
		}
		return Range{Window: windowDuration}, nil
	}
	if !hasStart && !hasEnd {
		return Range{}, nil
	}
	if !hasStart || !hasEnd || start == "" || end == "" {
		return Range{}, fmt.Errorf("custom history range requires both start and end RFC3339 timestamps")
	}
	startTime, parseErr := time.Parse(time.RFC3339, start)
	if parseErr != nil {
		return Range{}, fmt.Errorf("start must be an RFC3339 timestamp")
	}
	endTime, parseErr := time.Parse(time.RFC3339, end)
	if parseErr != nil {
		return Range{}, fmt.Errorf("end must be an RFC3339 timestamp")
	}
	if !endTime.After(startTime) {
		return Range{}, fmt.Errorf("end must be after start")
	}
	if endTime.Sub(startTime) > MaxWindow {
		return Range{}, fmt.Errorf("custom history range must not exceed %s", MaxWindow)
	}
	return Range{Window: endTime.Sub(startTime), Start: startTime.UTC(), End: endTime.UTC()}, nil
}

func singleValue(q url.Values, name string) (string, bool, error) {
	values, ok := q[name]
	if !ok {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", true, fmt.Errorf("%s must be specified exactly once", name)
	}
	return strings.TrimSpace(values[0]), true, nil
}
