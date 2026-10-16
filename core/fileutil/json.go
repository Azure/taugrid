// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fileutil

import "encoding/json"

func WriteJSONFileAtomic(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return WriteFileAtomic(path, raw, 0o644)
}
