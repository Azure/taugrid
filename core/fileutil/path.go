// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fileutil

import (
	"strings"
)

func ShortStringHash(value string) string {
	return ShortDigest(SHA256Hex([]byte(value)), 16)
}

func SafePathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return ShortStringHash(value)
	}
	return out
}
