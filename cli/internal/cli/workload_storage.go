package cli

import (
	"fmt"
	"strings"
)

func cleanStorageFlag(flag, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	clean := strings.TrimSpace(value)
	if clean == "" || clean != value {
		return "", fmt.Errorf("%s must not be empty or have surrounding whitespace", flag)
	}
	return clean, nil
}
