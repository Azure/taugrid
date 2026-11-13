// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobrender

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
)

func hotVolumeProfile() profile.Profile {
	p := trainProfile()
	p.HotVolume = &profile.HotVolume{StorageClassName: "tau-hot", DefaultSize: "1Ti", MaxSize: "2Ti"}
	return p
}

func renderedHotVolumePod(t *testing.T, o Options) map[string]any {
	t.Helper()
	out, err := Render(hotVolumeProfile(), o)
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	return m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func ephemeralClaimSpec(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	hot := podVolume(pod, "tau-hot")
	if hot == nil {
		t.Fatalf("tau-hot volume missing: %+v", pod["volumes"])
	}
	if _, ok := hot["emptyDir"]; ok {
		t.Fatalf("tau-hot is an emptyDir on a hot-volume profile: %+v", hot)
	}
	ephemeral, ok := hot["ephemeral"].(map[string]any)
	if !ok {
		t.Fatalf("tau-hot is not an ephemeral volume: %+v", hot)
	}
	return ephemeral["volumeClaimTemplate"].(map[string]any)["spec"].(map[string]any)
}

func podFSGroup(pod map[string]any) (string, string) {
	context, _ := pod["securityContext"].(map[string]any)
	if context == nil || context["fsGroup"] == nil {
		return "", ""
	}
	policy, _ := context["fsGroupChangePolicy"].(string)
	return fmt.Sprint(context["fsGroup"]), policy
}

func TestRender_HotVolumeProfileRendersEphemeralClaim(t *testing.T) {
	tests := []struct {
		name     string
		hotSize  string
		pvc      string
		wantSize string
	}{
		{name: "default size without durable data", wantSize: "1Ti"},
		{name: "hot_size with durable data", hotSize: "500Gi", pvc: "blob-training", wantSize: "500Gi"},
		{name: "hot_size equal to maxSize", hotSize: "2Ti", wantSize: "2Ti"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := renderedHotVolumePod(t, Options{
				Name:      "hot",
				Namespace: "tau",
				Command:   []string{"python", "train.py"},
				PVCMount:  tt.pvc,
				HotSize:   tt.hotSize,
			})
			spec := ephemeralClaimSpec(t, pod)
			want := map[string]any{
				"accessModes":      []any{"ReadWriteOnce"},
				"storageClassName": "tau-hot",
				"resources":        map[string]any{"requests": map[string]any{"storage": tt.wantSize}},
			}
			if !reflect.DeepEqual(spec, want) {
				t.Fatalf("tau-hot claim spec = %+v, want %+v", spec, want)
			}
			if got := containerMountPath(pod, "tau-hot"); got != "/mnt" {
				t.Fatalf("tau-hot mountPath = %q, want /mnt", got)
			}
			if got, ok := containerEnv(pod, "TAU_HOT_DIR"); !ok || got != "/mnt" {
				t.Fatalf("TAU_HOT_DIR = %q (set=%v), want /mnt", got, ok)
			}
			if group, policy := podFSGroup(pod); group != "65532" || policy != "OnRootMismatch" {
				t.Fatalf("pod fsGroup = %q (%q), want 65532 with OnRootMismatch", group, policy)
			}
		})
	}
}

func TestRender_HotVolumeProfileKeepsFSGroupInRestrictedMode(t *testing.T) {
	pod := renderedHotVolumePod(t, Options{
		Name:         "hot",
		Namespace:    "tau",
		Command:      []string{"python", "train.py"},
		SecurityMode: runconfig.SecurityModeRestricted,
	})
	if group, policy := podFSGroup(pod); group != "65532" || policy != "OnRootMismatch" {
		t.Fatalf("pod fsGroup = %q (%q), want 65532 with OnRootMismatch", group, policy)
	}
	if pod["securityContext"].(map[string]any)["runAsNonRoot"] != true {
		t.Fatalf("restricted pod securityContext = %+v, want runAsNonRoot", pod["securityContext"])
	}
}

func TestRender_HotVolumeProfileRejectsHotSizeAboveMaxSize(t *testing.T) {
	_, err := Render(hotVolumeProfile(), Options{
		Name:      "hot",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		HotSize:   "3Ti",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds the profile's hotVolume.maxSize 2Ti") {
		t.Fatalf("Render error = %v, want maxSize rejection", err)
	}
}

func TestRender_HotVolumeProfileKeepsCustomMntMount(t *testing.T) {
	pod := renderedHotVolumePod(t, Options{
		Name:         "hot",
		Namespace:    "tau",
		Command:      []string{"python", "train.py"},
		Volumes:      []Volume{{Name: "scratch", PVC: "scratch-pvc"}},
		VolumeMounts: []VolumeMount{{Name: "scratch", MountPath: "/mnt"}},
	})
	if podVolume(pod, "tau-hot") != nil {
		t.Fatalf("tau-hot mounted over custom /mnt mount: %+v", pod["volumes"])
	}
	if got := containerMountPath(pod, "scratch"); got != "/mnt" {
		t.Fatalf("scratch mountPath = %q, want /mnt", got)
	}
	if group, _ := podFSGroup(pod); group != "" {
		t.Fatalf("pod fsGroup = %q without a hot claim, want unset", group)
	}
}

func TestRender_HotVolumeJobUsesShortTTL(t *testing.T) {
	custom := Options{
		Volumes:      []Volume{{Name: "scratch", PVC: "scratch-pvc"}},
		VolumeMounts: []VolumeMount{{Name: "scratch", MountPath: "/mnt"}},
	}
	tests := []struct {
		name     string
		profile  profile.Profile
		ttl      int64
		explicit bool
		base     Options
		want     string
	}{
		{name: "hot claim without ttl", profile: hotVolumeProfile(), want: "600"},
		{name: "hot claim with workspace default", profile: hotVolumeProfile(), ttl: 86400, want: "600"},
		{name: "hot claim with explicit ttl", profile: hotVolumeProfile(), ttl: 3600, explicit: true, want: "3600"},
		{name: "custom /mnt keeps default", profile: hotVolumeProfile(), base: custom, want: "28800"},
		{name: "no hot volume keeps default", profile: trainProfile(), want: "28800"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := tt.base
			o.Name = "hot"
			o.Namespace = "tau"
			o.Command = []string{"python", "train.py"}
			o.TTLSecondsAfterFinished = tt.ttl
			o.TTLExplicit = tt.explicit
			out, err := Render(tt.profile, o)
			if err != nil {
				t.Fatal(err)
			}
			spec := parseYAML(t, out)["spec"].(map[string]any)
			if got := fmt.Sprint(spec["ttlSecondsAfterFinished"]); got != tt.want {
				t.Fatalf("ttlSecondsAfterFinished = %s, want %s", got, tt.want)
			}
		})
	}
}
