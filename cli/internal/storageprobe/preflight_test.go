// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storageprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScriptAvoidsAwkDependency(t *testing.T) {
	script := Script()
	if strings.Contains(script, "awk ") {
		t.Fatalf("storage preflight should not require awk:\n%s", script)
	}
}

func TestScriptWarnsWhenDfUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test depends on POSIX shell PATH semantics")
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	binDir := t.TempDir()
	for _, name := range []string{"mkdir", "rm"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not available", name)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	hotDir := filepath.Join(t.TempDir(), "hot")
	if err := os.Mkdir(hotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "-c", Script())
	cmd.Env = []string{
		"PATH=" + binDir,
		"TAU_HOT_DIR=" + hotDir,
		"TAU_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage preflight should continue without df; err=%v output=%s", err, out)
	}
	if !strings.Contains(string(out), "tau storage warning: df not found; skipping free-space check for "+hotDir) {
		t.Fatalf("missing df warning:\n%s", out)
	}
}

func runPreflight(t *testing.T, env []string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test depends on POSIX shell semantics")
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command(bashPath, "-c", Script()+"\necho \"status=$TAU_STORAGE_HOT_STATUS hot_dir=$TAU_HOT_DIR reason=$TAU_STORAGE_HOT_REASON\"")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TAU_HOT_PROBE_MIB=1", "TAU_HOT_MIN_WRITE_MBPS=0", "TAU_HOT_REQUIRED_KIB=0"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func hotFixture(t *testing.T, mounts func(hot string) string) []string {
	t.Helper()
	hot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	table := "25 1 0:30 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n" +
		"30 25 8:1 /var/lib/kubelet/pods/p/etc-hosts /etc/hosts rw,relatime - ext4 /dev/sda1 rw\n" +
		mounts(hot)
	if err := os.WriteFile(mountinfo, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{
		"TAU_HOT_DIR=" + hot,
		"TAU_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"TAU_HOT_MOUNTINFO=" + mountinfo,
	}
}

func TestScriptReportsHotBacking(t *testing.T) {
	cases := []struct {
		name    string
		mounts  func(hot string) string
		require bool
		wantErr bool
		want    []string
	}{
		{
			name: "logical volume",
			mounts: func(hot string) string {
				return "31 25 253:3 / " + hot + " rw,relatime shared:5 - xfs /dev/topolvm/vol-1 rw,prjquota\n"
			},
			require: true,
			want:    []string{"source=/dev/topolvm/vol-1 fstype=xfs", "backing=dedicated", "status=hot"},
		},
		{
			name: "emptyDir on the kubelet disk",
			mounts: func(hot string) string {
				return "32 25 8:1 /var/lib/kubelet/pods/p/volumes/kubernetes.io~empty-dir/tau-hot " + hot + " rw,relatime - ext4 /dev/sda1 rw\n"
			},
			require: true,
			wantErr: true,
			want:    []string{"source=/dev/sda1 fstype=ext4", "backing=kubelet-disk", "tau storage error: ", "is on the node's kubelet disk; TAU_HOT_REQUIRE_LOCAL=1 forbids falling back"},
		},
		{
			name:    "directory on the container root",
			mounts:  func(string) string { return "" },
			require: true,
			wantErr: true,
			want:    []string{"source=overlay fstype=overlay", "backing=container-root", "is on the container root filesystem;"},
		},
		{
			name: "kubelet disk without require",
			mounts: func(hot string) string {
				return "32 25 8:1 /var/lib/kubelet/pods/p/volumes/kubernetes.io~empty-dir/tau-hot " + hot + " rw,relatime - ext4 /dev/sda1 rw\n"
			},
			want: []string{"backing=kubelet-disk", "status=hot"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := hotFixture(t, tc.mounts)
			if tc.require {
				env = append(env, "TAU_HOT_REQUIRE_LOCAL=1")
			}
			out, err := runPreflight(t, env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v\n%s", err, tc.wantErr, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			if tc.wantErr && strings.Contains(out, "status=") {
				t.Errorf("preflight should stop before the workload:\n%s", out)
			}
		})
	}
}

func TestScriptFreeSpaceCheck(t *testing.T) {
	if _, err := exec.LookPath("df"); err != nil {
		t.Skip("df not available")
	}
	dedicated := func(hot string) string {
		return "31 25 253:3 / " + hot + " rw,relatime - xfs /dev/topolvm/vol-1 rw\n"
	}
	env := append(hotFixture(t, dedicated), "TAU_HOT_REQUIRED_KIB=999999999999")
	out, err := runPreflight(t, env)
	if err != nil {
		t.Fatalf("fallback should not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "status=durable-fallback") || !strings.Contains(out, "reason=insufficient free space") {
		t.Fatalf("expected free-space fallback:\n%s", out)
	}
	if strings.Contains(out, "avail_kib=unknown") {
		t.Fatalf("df free space was not parsed:\n%s", out)
	}

	env = append(hotFixture(t, dedicated), "TAU_HOT_REQUIRED_KIB=999999999999", "TAU_HOT_REQUIRE_LOCAL=1")
	out, err = runPreflight(t, env)
	if err == nil || !strings.Contains(out, "tau storage error: ") || !strings.Contains(out, "insufficient free space") {
		t.Fatalf("require-local should fail on low free space: err=%v\n%s", err, out)
	}
}

func TestScriptRequireLocalFailsWhenHotDirMissing(t *testing.T) {
	out, err := runPreflight(t, []string{
		"TAU_HOT_DIR=" + filepath.Join(t.TempDir(), "absent"),
		"TAU_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"TAU_HOT_REQUIRE_LOCAL=1",
	})
	if err == nil || !strings.Contains(out, "is missing; TAU_HOT_REQUIRE_LOCAL=1 forbids falling back") {
		t.Fatalf("require-local should fail on a missing hot dir: err=%v\n%s", err, out)
	}
}

func TestScriptRequireLocalFailsWhenHotDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	hot := filepath.Join(t.TempDir(), "hot")
	if err := os.Mkdir(hot, 0o555); err != nil {
		t.Fatal(err)
	}
	out, err := runPreflight(t, []string{
		"TAU_HOT_DIR=" + hot,
		"TAU_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"TAU_HOT_REQUIRE_LOCAL=1",
	})
	if err == nil || !strings.Contains(out, "is not writable; TAU_HOT_REQUIRE_LOCAL=1 forbids falling back") {
		t.Fatalf("require-local should fail on a read-only hot dir: err=%v\n%s", err, out)
	}
}
