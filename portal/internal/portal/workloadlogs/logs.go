// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadlogs

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	DefaultTailLines  int64 = 200
	MaxTailLines      int64 = 1000
	DefaultLimitBytes int64 = 256 * 1024
	MaxLimitBytes     int64 = 1024 * 1024
)

type Snapshot struct {
	Pod              string `json:"pod"`
	Container        string `json:"container"`
	Previous         bool   `json:"previous"`
	Content          string `json:"content"`
	TailLines        int64  `json:"tailLines"`
	LimitBytes       int64  `json:"limitBytes"`
	Truncated        bool   `json:"truncated"`
	RedactionApplied bool   `json:"redactionApplied"`
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?)[^\s]+`),
	regexp.MustCompile(`(?i)([?&](?:sig|token|key|secret|password)=)[^&\s]+`),
	regexp.MustCompile(`(?i)\b(accountkey|sharedaccesskey|client_secret|password|token|secret|sig)\s*[:=]\s*[^\s,;]+`),
}

func Sanitize(data []byte, limit int64) (content string, truncated, redacted bool) {
	if limit < 0 {
		limit = 0
	}
	if int64(len(data)) > limit {
		data = data[:limit]
		truncated = true
	}
	content = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		redacted = true
		return -1
	}, string(data))
	for _, pattern := range secretPatterns {
		next := pattern.ReplaceAllStringFunc(content, func(value string) string {
			redacted = true
			if i := strings.IndexAny(value, ":="); i >= 0 {
				return value[:i+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
		content = next
	}
	return content, truncated, redacted
}
