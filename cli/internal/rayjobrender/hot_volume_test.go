// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

import (
	"reflect"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
)

func hotVolumeOptions(gpusPerWorker int) Options {
	o := archiveOptions()
	o.GPUsPerWorker = gpusPerWorker
	o.Profile.HotVolume = &profile.HotVolume{StorageClassName: "tau-hot", DefaultSize: "1Ti", MaxSize: "2Ti"}
	return o
}

func tauHotVolume(t *testing.T, role string, pod map[string]any) map[string]any {
	t.Helper()
	for _, item := range pod["volumes"].([]any) {
		v := item.(map[string]any)
		if v["name"] == "tau-hot" {
			return v
		}
	}
	t.Fatalf("%s pod has no tau-hot volume: %+v", role, pod["volumes"])
	return nil
}

func TestRenderHotVolumeProfileGivesWorkersEphemeralClaims(t *testing.T) {
	tests := []struct {
		name          string
		gpusPerWorker int
		hotSize       string
		wantSize      string
		wantHeadLimit any
	}{
		{name: "GPU workers with default size", gpusPerWorker: 1, wantSize: "1Ti"},
		{name: "GPU workers with hot_size", gpusPerWorker: 8, hotSize: "500Gi", wantSize: "500Gi", wantHeadLimit: "500Gi"},
		{name: "CPU workers", wantSize: "1Ti"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := hotVolumeOptions(tt.gpusPerWorker)
			o.HotSize = tt.hotSize
			for role, pod := range rayPodSpecs(t, renderWithArchive(t, o)) {
				hot := tauHotVolume(t, role, pod)
				if role == "head" {
					emptyDir, ok := hot["emptyDir"].(map[string]any)
					if !ok || hot["ephemeral"] != nil || emptyDir["sizeLimit"] != tt.wantHeadLimit {
						t.Fatalf("head tau-hot = %+v, want emptyDir with sizeLimit %v", hot, tt.wantHeadLimit)
					}
					continue
				}
				if _, ok := hot["emptyDir"]; ok {
					t.Fatalf("%s tau-hot is an emptyDir on a hot-volume profile: %+v", role, hot)
				}
				ephemeral, ok := hot["ephemeral"].(map[string]any)
				if !ok {
					t.Fatalf("%s tau-hot is not an ephemeral volume: %+v", role, hot)
				}
				spec := ephemeral["volumeClaimTemplate"].(map[string]any)["spec"]
				want := map[string]any{
					"accessModes":      []any{"ReadWriteOnce"},
					"storageClassName": "tau-hot",
					"resources":        map[string]any{"requests": map[string]any{"storage": tt.wantSize}},
				}
				if !reflect.DeepEqual(spec, want) {
					t.Fatalf("%s tau-hot claim spec = %+v, want %+v", role, spec, want)
				}
			}
		})
	}
}

func TestRenderHotVolumeProfileRejectsHotSizeAboveMaxSize(t *testing.T) {
	o := hotVolumeOptions(1)
	o.HotSize = "3Ti"
	if _, err := Render(o); err == nil || !strings.Contains(err.Error(), "exceeds the profile's hotVolume.maxSize 2Ti") {
		t.Fatalf("Render error = %v, want maxSize rejection", err)
	}
}
