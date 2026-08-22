// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
)

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
