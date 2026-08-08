// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fileutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

func SHA256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func ShortDigest(digest string, length int) string {
	if length <= 0 || len(digest) <= length {
		return digest
	}
	return digest[:length]
}
