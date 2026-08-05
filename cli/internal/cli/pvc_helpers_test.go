package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestShellSingleQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"foo", "'foo'"},
		{"/data/checkpoints/finetunes/run-1/metrics.json", "'/data/checkpoints/finetunes/run-1/metrics.json'"},
		{"a$b`c\\d", "'a$b`c\\d'"},       // dollar / backtick / backslash all neutralized inside single quotes
		{"it's", `'it'\''s'`},            // embedded single quote uses canonical escape
		{"$(rm -rf /)", "'$(rm -rf /)'"}, // command substitution is inert inside single quotes
		{"--field=$(injected)", "'--field=$(injected)'"},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHelperPodYAML_RoundTrip(t *testing.T) {
	out, err := helperPodYAML(helperPodSpec{
		Name:      "tau-get-foo-12345",
		Namespace: "team-a",
		LabelApp:  "tau-pvc-get",
		Image:     "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
		PVCName:   "blob-training",
		TTLSec:    60,
		Script:    "cat 'foo'\n",
	})
	if err != nil {
		t.Fatalf("helperPodYAML: %v", err)
	}
	var got helperPodObject
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if got.APIVersion != "v1" || got.Kind != "Pod" {
		t.Errorf("apiVersion/kind: %q/%q", got.APIVersion, got.Kind)
	}
	if got.Metadata.Name != "tau-get-foo-12345" || got.Metadata.Namespace != "team-a" {
		t.Errorf("metadata: %+v", got.Metadata)
	}
	if got.Spec.RestartPolicy != "Never" || got.Spec.ActiveDeadlineSeconds != 60 {
		t.Errorf("podSpec scalars: %+v", got.Spec)
	}
	if len(got.Spec.Containers) != 1 || got.Spec.Containers[0].Image != "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662" {
		t.Errorf("container: %+v", got.Spec.Containers)
	}
	if len(got.Spec.Volumes) != 1 || got.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "blob-training" {
		t.Errorf("volumes: %+v", got.Spec.Volumes)
	}
}

func TestHelperPodYAML_ResistsYAMLInjection(t *testing.T) {
	// A path supplied via the tau.azure.com/result-path Job annotation
	// could in theory contain YAML metacharacters. The previous
	// implementation text-templated the whole Pod YAML, so a crafted path
	// could close the args block and inject a sibling key (hostNetwork,
	// serviceAccountName, hostPID, etc.). The structural fix is that
	// helperPodObject's Go type has no fields for those keys, so
	// yaml.Marshal physically cannot emit them at any nesting depth —
	// regardless of what the caller puts in Script. This test feeds an
	// adversarial Script and verifies the byte-stream output:
	//   (a) parses back into helperPodObject (it's well-formed YAML),
	//   (b) lands the hostile bytes inside args[0] (where the API server
	//       treats them as opaque sh -c content),
	//   (c) does not contain hostNetwork/hostPID/serviceAccountName etc.
	//       at column 0 (top-level) or column 2 (spec-level), which is
	//       the only place those keys would have privileged effect.
	hostile := "cat /etc/passwd\nhostNetwork: true\nserviceAccountName: cluster-admin\nhostPID: true\n# trailing"
	out, err := helperPodYAML(helperPodSpec{
		Name:      "tau-ls-hostile",
		Namespace: "default",
		LabelApp:  "tau-pvc-list",
		Image:     pvcHelperImage,
		PVCName:   "blob-training",
		TTLSec:    60,
		Script:    hostile,
	})
	if err != nil {
		t.Fatalf("helperPodYAML: %v", err)
	}

	var typed helperPodObject
	if err := yaml.Unmarshal(out, &typed); err != nil {
		t.Fatalf("re-parse typed: %v\n%s", err, out)
	}
	if len(typed.Spec.Containers) != 1 || len(typed.Spec.Containers[0].Args) != 1 {
		t.Fatalf("want 1 container with 1 arg, got %+v", typed.Spec.Containers)
	}
	if !strings.Contains(typed.Spec.Containers[0].Args[0], "hostNetwork: true") {
		t.Errorf("hostile content not preserved in args: %q", typed.Spec.Containers[0].Args[0])
	}

	// Line-by-line scan: the privileged keys must never appear at
	// column 0 (top-level Pod field) or column 2/4 (spec-level field).
	// Inside an indented block scalar they're inert sh -c argument bytes.
	for _, line := range strings.Split(string(out), "\n") {
		for _, key := range []string{"hostNetwork:", "hostPID:", "hostIPC:", "serviceAccountName:", "automountServiceAccountToken:"} {
			// match at column 0 or column 2 or column 4 only
			for _, prefix := range []string{key, "  " + key, "    " + key} {
				if line == prefix || strings.HasPrefix(line, prefix+" ") {
					t.Errorf("privileged key %q leaked as a structural YAML field: %q", key, line)
				}
			}
		}
	}
}

func TestPVCHelperPodNamePreservesUniqueSuffixForLongRuns(t *testing.T) {
	runName := strings.Repeat("very-long-run-", 8)
	first, err := pvcHelperPodName("tau-get", runName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pvcHelperPodName("tau-get", runName)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) > 63 || len(second) > 63 {
		t.Fatalf("helper names exceed DNS limit: %d %d", len(first), len(second))
	}
	if first == second {
		t.Fatalf("helper names collided: %q", first)
	}
}

func TestDecodePVCListLogsRequiresCompletionMarker(t *testing.T) {
	_, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		pvcHelperListCompleteMarker+":tau-ls-chaos",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "missing its completion marker") {
		t.Fatalf("missing-marker error = %v", err)
	}
}

func TestDecodePVCListLogsAcceptsCompletedZeroEntryListing(t *testing.T) {
	marker := pvcHelperListCompleteMarker + ":tau-ls-chaos"
	entries, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		marker,
		marker+"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want none", entries)
	}
}

func TestDecodePVCListLogsParsesEntriesBeforeCompletionMarker(t *testing.T) {
	marker := pvcHelperListCompleteMarker + ":tau-ls-chaos"
	encoded := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	entries, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		marker,
		encoded("evidence_w128.json")+"\n"+
			encoded("rollout_w128.png")+"\n"+
			encoded("summary_w128.json")+"\n"+
			marker+"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"evidence_w128.json", "rollout_w128.png", "summary_w128.json"}
	if strings.Join(entries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
}

func TestPVCListHelperEnumeratesNestedDirectory(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"metrics.json":                    `{"loss":1.25}`,
		"checkpoint/metadata/_METADATA":   "metadata",
		"checkpoint/state/array_0/shard0": "weights",
	} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs, stderr, err := runPVCListHelper(root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"checkpoint",
		"checkpoint/metadata",
		"checkpoint/metadata/_METADATA",
		"checkpoint/state",
		"checkpoint/state/array_0",
		"checkpoint/state/array_0/shard0",
		"metrics.json",
	}
	if strings.Join(entries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries:\n%q\nwant:\n%q", entries, want)
	}
}

func TestPVCListHelperKeepsModelAndDatasetListingsShallow(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"models/model-a/runs/run-1.json":         "{}",
		"datasets/fineweb/versions/v1/data.json": "{}",
	} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, dir := range []string{"models", "datasets"} {
		listRoot := filepath.Join(root, dir)
		logs, stderr, err := runPVCListHelper(listRoot, 0, nil, false)
		if err != nil {
			t.Fatalf("%s helper failed: %v\nstderr:\n%s", dir, err, stderr)
		}
		entries, err := decodePVCListLogs(listRoot, "test-pvc", "test-complete", logs)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"models": "model-a", "datasets": "fineweb"}[dir]
		if strings.Join(entries, "\n") != want {
			t.Fatalf("%s entries = %q, want only direct child %q", dir, entries, want)
		}
	}
}

func TestPVCListHelperEnumeratesTrailingSlashPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, stderr, err := runPVCListHelper(root+string(os.PathSeparator), 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entries, "\n") != "artifact.json" {
		t.Fatalf("entries = %q, want relative artifact.json", entries)
	}
}

func TestPVCListHelperAcceptsGenuinelyEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	logs, stderr, err := runPVCListHelper(root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %q, want none", entries)
	}
}

func TestPVCListHelperRetriesAfterSuppressedInitialListing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	realFind, err := exec.LookPath("find")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	findState := filepath.Join(t.TempDir(), "find-state")
	sleepState := filepath.Join(t.TempDir(), "sleep-state")
	fakeFind := filepath.Join(binDir, "find")
	findScript := `#!/bin/sh
if [ ! -e "$TAU_TEST_FIND_STATE" ]; then
  : >"$TAU_TEST_FIND_STATE"
  exit 0
fi
if [ ! -e "$TAU_TEST_SLEEP_STATE" ]; then
  exit 8
fi
exec "$TAU_TEST_REAL_FIND" "$@"
`
	if err := os.WriteFile(fakeFind, []byte(findScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeSleep := filepath.Join(binDir, "sleep")
	if err := os.WriteFile(fakeSleep, []byte("#!/bin/sh\n: >\"$TAU_TEST_SLEEP_STATE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"TAU_TEST_FIND_STATE="+findState,
		"TAU_TEST_SLEEP_STATE="+sleepState,
		"TAU_TEST_REAL_FIND="+realFind,
	)

	logs, stderr, err := runPVCListHelper(root, 12, env, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entries, "\n") != "artifact.json" {
		t.Fatalf("entries = %q, want artifact.json", entries)
	}
}

func TestPVCListHelperRejectsDirectFilePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(file, []byte(`{"loss":1.25}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runPVCListHelper(file, 0, nil, true)
	if err == nil {
		t.Fatal("helper unexpectedly treated a direct file path as a directory")
	}
	if !strings.Contains(stderr, pvcHelperNotFoundMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperNotFoundMarker)
	}
}

func TestPVCListHelperRejectsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, stderr, err := runPVCListHelper(missing, 0, nil, true)
	if err == nil {
		t.Fatal("helper unexpectedly accepted a missing path")
	}
	if !strings.Contains(stderr, pvcHelperNotFoundMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperNotFoundMarker)
	}
}

func TestPVCListHelperPreservesSpecialFilenames(t *testing.T) {
	root := t.TempDir()
	names := []string{
		" leading-space",
		"-leading-dash",
		"embedded\nnewline",
		"quote'and$dollar",
		"subdir/tab\tname",
	}
	for _, name := range names {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs, stderr, err := runPVCListHelper(root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), names...)
	want = append(want, "subdir")
	sort.Strings(want)
	if strings.Join(entries, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("entries = %q, want %q", entries, want)
	}
}

func TestPVCListHelperReportsEnumerationFailure(t *testing.T) {
	binDir := t.TempDir()
	fakeFind := filepath.Join(binDir, "find")
	if err := os.WriteFile(fakeFind, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))

	_, stderr, err := runPVCListHelper(t.TempDir(), 0, env, true)
	if err == nil {
		t.Fatal("helper unexpectedly accepted a failed directory probe")
	}
	if !strings.Contains(stderr, pvcHelperListFailedMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperListFailedMarker)
	}
}

func runPVCListHelper(dir string, settleSeconds int, env []string, recursive bool) (string, string, error) {
	cmd := exec.Command(
		"sh",
		"-c",
		pvcListHelperScript(recursive),
		"helper",
		dir,
		"test-complete",
		strconv.Itoa(settleSeconds),
	)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
