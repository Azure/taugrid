package workspaceconnection

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

type fakeCredentialProvider struct {
	calls int
	raw   []byte
	err   error
}

func (f *fakeCredentialProvider) UserKubeconfig(context.Context, Descriptor) ([]byte, error) {
	f.calls++
	return append([]byte(nil), f.raw...), f.err
}

type fakeVerifier struct {
	calls  int
	result Verification
	err    error
	paths  []string
	modes  []os.FileMode
}

func (f *fakeVerifier) Verify(_ context.Context, _ Descriptor, kubeconfigPath string) (Verification, error) {
	f.calls++
	f.paths = append(f.paths, kubeconfigPath)
	if info, err := os.Stat(kubeconfigPath); err == nil {
		f.modes = append(f.modes, info.Mode().Perm())
	}
	if f.err != nil {
		return Verification{}, f.err
	}
	result := f.result
	if result.WorkspaceUID == "" {
		result.WorkspaceUID = "workspace-uid"
	}
	return result, nil
}

func writeDescriptorFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestManagerConfiguresFirstConnectionNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{result: Verification{
		ContextName:       "taugrid-flex",
		Namespace:         "sample",
		Queue:             "jobqueue",
		WorkspacePhase:    "Ready",
		WorkspaceRevision: "7",
	}}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return now },
	}

	connection, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if connection.Workspace != "sample" ||
		connection.Namespace != "sample" ||
		connection.Queue != "jobqueue" ||
		connection.AuthorizationMode != AuthorizationModeClusterWide {
		t.Fatalf("connection = %#v", connection)
	}
	if connection.ConfiguredAt != now || connection.VerifiedAt != now {
		t.Fatalf("connection timestamps = %#v", connection)
	}
	info, err := os.Stat(connection.KubeconfigPath)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")); err != nil {
		t.Fatalf("connection state missing: %v", err)
	}
	state, err := loadConnectionState(filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Schema != connectionStateSchema ||
		state.ConfiguredAt != now ||
		state.VerifiedAt != now ||
		state.Provider != "azure" ||
		state.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("persisted configuration/readiness state = %#v", state)
	}
}

func TestManagerFirstConnectionCredentialFailureLeavesNoStateOrKubeconfig(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{err: errors.New("noninteractive Azure identity unavailable")}
	verifier := &fakeVerifier{}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := manager.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "noninteractive Azure identity unavailable") {
		t.Fatalf("expected credential failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 0 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	for _, path := range []string{
		filepath.Join(configDir, "connections"),
		filepath.Join(configDir, "kubeconfigs"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("credential failure left residue at %s: %v", path, statErr)
		}
	}
}

func TestManagerFirstConnectionAuthorizationFailureLeavesNoTrustedState(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{err: errors.New("cluster-user credential is not authorized")}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := manager.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected authorization failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("authorization failure left trusted state at %s: %v", statePath, statErr)
	}
	kubeconfigEntries, readErr := os.ReadDir(filepath.Join(configDir, "kubeconfigs"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(kubeconfigEntries) != 0 {
		t.Fatalf("authorization failure left kubeconfig residue: %v", kubeconfigEntries)
	}
}

func TestManagerReusesFreshConnectionNonInteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	firstCredentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	firstVerifier := &fakeVerifier{result: Verification{
		ContextName:       "taugrid-flex",
		Namespace:         "sample",
		Queue:             "jobqueue",
		WorkspacePhase:    "Ready",
		WorkspaceRevision: "7",
	}}
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: firstCredentials,
		Verifier:    firstVerifier,
		Now:         func() time.Time { return now },
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	secondCredentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	secondVerifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: secondCredentials,
		Verifier:    secondVerifier,
		Now:         func() time.Time { return now.Add(time.Minute) },
	}
	if _, err := second.Ensure(context.Background(), root); err != nil {
		t.Fatalf("fresh cached Ensure: %v", err)
	}
	if secondCredentials.calls != 0 || secondVerifier.calls != 0 {
		t.Fatalf("fresh cache made credentials=%d verifier=%d calls", secondCredentials.calls, secondVerifier.calls)
	}
}

func TestManagerRevalidatesStaleConnectionWithoutRefetchingCredentials(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}

	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready", WorkspaceRevision: "8",
	}}
	second := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Credentials:  credentials,
		Verifier:     verifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	got, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("stale cached Ensure: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 1 {
		t.Fatalf("stale cache made credentials=%d verifier=%d calls", credentials.calls, verifier.calls)
	}
	if got.WorkspaceRevision != "8" {
		t.Fatalf("workspace revision = %q, want 8", got.WorkspaceRevision)
	}
	if got.ConfiguredAt != now {
		t.Fatalf("configuration timestamp changed during readiness refresh: %s", got.ConfiguredAt)
	}
}

func TestManagerReacquiresMissingKubeconfigNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			ServiceAccount: "workspace-sa", WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := first.Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}

	refreshedAt := configuredAt.Add(10 * time.Minute)
	replacementRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: replacement\n")
	credentials := &fakeCredentialProvider{raw: replacementRaw}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		ServiceAccount: "workspace-sa", WorkspacePhase: "Ready", WorkspaceRevision: "8",
	}}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return refreshedAt },
	}
	got, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("reacquire missing kubeconfig: %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if len(verifier.paths) != 1 ||
		verifier.paths[0] == got.KubeconfigPath ||
		len(verifier.modes) != 1 ||
		verifier.modes[0] != 0o600 {
		t.Fatalf("candidate paths=%v modes=%v final=%s", verifier.paths, verifier.modes, got.KubeconfigPath)
	}
	if _, statErr := os.Stat(verifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("verified candidate was not removed: %v", statErr)
	}
	if got.ConfiguredAt != configuredAt || got.VerifiedAt != refreshedAt {
		t.Fatalf("connection timestamps = %#v", got)
	}
	if got.WorkspaceUID != "workspace-uid" ||
		got.Namespace != "sample" ||
		got.Queue != "jobqueue" ||
		got.ServiceAccount != "workspace-sa" {
		t.Fatalf("reacquired connection = %#v", got)
	}
	info, err := os.Stat(got.KubeconfigPath)
	if err != nil {
		t.Fatalf("reacquired kubeconfig: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
	finalRaw, err := os.ReadFile(got.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finalRaw, replacementRaw) {
		t.Fatalf("final kubeconfig = %q, want verified replacement %q", finalRaw, replacementRaw)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ConfiguredAt != configuredAt ||
		state.VerifiedAt != refreshedAt ||
		state.KubeconfigPath != got.KubeconfigPath {
		t.Fatalf("persisted reacquired state = %#v", state)
	}
}

func TestManagerMissingKubeconfigRejectsLiveDriftWithoutCredentialResidue(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			ServiceAccount: "workspace-sa", WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := first.Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
		ServiceAccount: "replacement-sa", WorkspaceUID: "replacement-uid",
		WorkspacePhase: "Ready", WorkspaceRevision: "1",
	}}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return configuredAt.Add(10 * time.Minute) },
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "workspace UID") ||
		!strings.Contains(err.Error(), "namespace") ||
		!strings.Contains(err.Error(), "LocalQueue") ||
		!strings.Contains(err.Error(), "service account") {
		t.Fatalf("expected reacquired connection drift failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if _, statErr := os.Stat(connection.KubeconfigPath); !os.IsNotExist(statErr) {
		t.Fatalf("live drift left reacquired kubeconfig: %v", statErr)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("live drift changed pinned state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}

	var out bytes.Buffer
	interactive := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
			ServiceAccount: "replacement-sa", WorkspaceUID: "replacement-uid",
			WorkspacePhase: "Ready", WorkspaceRevision: "1",
		}},
		Now: func() time.Time { return configuredAt.Add(11 * time.Minute) },
	}
	accepted, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("accept reacquired live drift: %v", err)
	}
	if accepted.ConfiguredAt != configuredAt.Add(11*time.Minute) ||
		accepted.WorkspaceUID != "replacement-uid" ||
		accepted.Namespace != "sample-v2" ||
		accepted.Queue != "priority" ||
		accepted.ServiceAccount != "replacement-sa" {
		t.Fatalf("accepted connection = %#v", accepted)
	}
	if !strings.Contains(out.String(), "Pin the updated namespace") {
		t.Fatalf("interactive drift output:\n%s", out.String())
	}
}

func TestManagerMissingKubeconfigVerificationFailureLeavesStateUntouched(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := first.Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{err: errors.New("workspace authorization verification failed")}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return configuredAt.Add(10 * time.Minute) },
	}
	_, err = second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "workspace authorization verification failed") {
		t.Fatalf("expected verifier failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if _, statErr := os.Stat(connection.KubeconfigPath); !os.IsNotExist(statErr) {
		t.Fatalf("verification failure left reacquired kubeconfig: %v", statErr)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("verification failure changed pinned state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}
}

func TestManagerMigratesV1ConfirmationOnlyAfterLiveVerification(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	legacyState := map[string]any{
		"schema":               connectionStateSchemaV1,
		"workspace":            state.Workspace,
		"resource_id":          state.ResourceID,
		"authorization_mode":   state.AuthorizationMode,
		"context_name":         state.ContextName,
		"kubeconfig_path":      state.KubeconfigPath,
		"namespace":            state.Namespace,
		"queue":                state.Queue,
		"service_account":      state.ServiceAccount,
		"required_role":        state.RequiredRole,
		"descriptor_path":      state.DescriptorPath,
		"descriptor_digest":    state.DescriptorDigest,
		"workspace_revision":   state.WorkspaceRevision,
		"confirmed_at":         now,
		"verified_at":          state.VerifiedAt,
		"repository_root":      state.RepositoryRoot,
		"private_cluster":      state.PrivateCluster,
		"network_instructions": state.NetworkInstructions,
	}
	if err := fileutil.WriteJSONFileAtomic(statePath, legacyState); err != nil {
		t.Fatal(err)
	}

	failedVerifier := &fakeVerifier{err: errors.New("workspace API unavailable")}
	failedMigration := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Verifier:     failedVerifier,
		Now:          func() time.Time { return now.Add(15 * time.Second) },
		ReadinessTTL: time.Minute,
	}
	if _, err := failedMigration.Ensure(context.Background(), root); err == nil ||
		!strings.Contains(err.Error(), "workspace API unavailable") {
		t.Fatalf("expected live verification failure, got %v", err)
	}
	unmigrated, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if unmigrated.Schema != connectionStateSchemaV1 ||
		!unmigrated.ConfiguredAt.IsZero() ||
		unmigrated.ConfirmedAt == nil ||
		*unmigrated.ConfirmedAt != now {
		t.Fatalf("v1 state migrated before verification: %#v", unmigrated)
	}
	rawBeforeMigration, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawBeforeMigration), "configured_at") {
		t.Fatalf("v1 state gained configured_at before verification:\n%s", rawBeforeMigration)
	}

	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready", WorkspaceRevision: "8",
	}}
	second := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Verifier:     verifier,
		Now:          func() time.Time { return now.Add(30 * time.Second) },
		ReadinessTTL: time.Minute,
	}
	got, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("migrate v1 configuration: %v", err)
	}
	if got.ConfiguredAt != now || got.WorkspaceUID != "workspace-uid" {
		t.Fatalf("migrated connection = %#v", got)
	}
	if verifier.calls != 1 {
		t.Fatalf("fresh v1 migration verifier calls = %d, want 1", verifier.calls)
	}
	migrated, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Schema != connectionStateSchema ||
		migrated.WorkspaceUID != "workspace-uid" ||
		migrated.Provider != "azure" ||
		migrated.TenantID != "11111111-1111-1111-1111-111111111111" ||
		migrated.ConfirmedAt != nil {
		t.Fatalf("migrated state = %#v", migrated)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "confirmed_at") || strings.Contains(string(raw), "approved_at") {
		t.Fatalf("migrated v2 state retained legacy timestamp fields:\n%s", raw)
	}
}

func TestManagerRejectsUIDLessV2StateNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now: func() time.Time { return now },
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.WorkspaceUID = ""
	if err := fileutil.WriteJSONFileAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}

	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier:    verifier,
		Now:         func() time.Time { return now.Add(30 * time.Second) },
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected UID-less v2 state to require interactive review, got %v", err)
	}
	if verifier.calls != 0 {
		t.Fatalf("UID-less v2 state was verified before review: calls=%d", verifier.calls)
	}
}

func TestManagerInteractiveReconfigureVerifierFailurePreservesExistingKubeconfig(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	originalRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: original\n")
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: originalRaw},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
	}
	connection, err := first.Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKey(discovery.Descriptor)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.WorkspaceUID = ""
	if err := fileutil.WriteJSONFileAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfigBefore, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}

	verifier := &fakeVerifier{err: errors.New("candidate verification failed")}
	var out bytes.Buffer
	second := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: &fakeCredentialProvider{raw: []byte("replacement credentials")},
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "candidate verification failed") {
		t.Fatalf("expected candidate verification failure, got %v", err)
	}
	if len(verifier.paths) != 1 ||
		verifier.paths[0] == connection.KubeconfigPath ||
		len(verifier.modes) != 1 ||
		verifier.modes[0] != 0o600 {
		t.Fatalf("candidate paths=%v modes=%v final=%s", verifier.paths, verifier.modes, connection.KubeconfigPath)
	}
	if _, statErr := os.Stat(verifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("failed candidate was not removed: %v", statErr)
	}
	kubeconfigAfter, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatalf("original kubeconfig was removed: %v", err)
	}
	if !bytes.Equal(kubeconfigBefore, kubeconfigAfter) || !bytes.Equal(kubeconfigAfter, originalRaw) {
		t.Fatalf("original kubeconfig changed:\nbefore=%q\nafter=%q", kubeconfigBefore, kubeconfigAfter)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("failed reconfigure changed state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}
	if !strings.Contains(out.String(), "workspace UID pin is missing") {
		t.Fatalf("interactive reconfigure output:\n%s", out.String())
	}

	replacementRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: replacement\n")
	successVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready", WorkspaceRevision: "8",
	}}
	successful := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: replacementRaw},
		Verifier:    successVerifier,
	}
	reconfigured, err := successful.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("successful staged reconfigure: %v", err)
	}
	if reconfigured.KubeconfigPath != connection.KubeconfigPath ||
		reconfigured.WorkspaceUID != "workspace-uid" {
		t.Fatalf("reconfigured connection = %#v", reconfigured)
	}
	if len(successVerifier.paths) != 1 ||
		successVerifier.paths[0] == connection.KubeconfigPath ||
		len(successVerifier.modes) != 1 ||
		successVerifier.modes[0] != 0o600 {
		t.Fatalf("successful candidate paths=%v modes=%v final=%s", successVerifier.paths, successVerifier.modes, connection.KubeconfigPath)
	}
	if _, statErr := os.Stat(successVerifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("successful candidate was not removed: %v", statErr)
	}
	replacedRaw, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replacedRaw, replacementRaw) {
		t.Fatalf("final kubeconfig = %q, want %q", replacedRaw, replacementRaw)
	}
}

func TestManagerRejectsDescriptorTrustChangeNonInteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DescriptorRelativePath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(
		string(raw),
		"tenantID: 11111111-1111-1111-1111-111111111111",
		"tenantID: 33333333-3333-3333-3333-333333333333",
		1,
	))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected changed descriptor to require review, got %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("changed trust fetched credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}

	var out bytes.Buffer
	interactiveCredentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	interactiveVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready", WorkspaceRevision: "8",
	}}
	interactive := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: interactiveCredentials,
		Verifier:    interactiveVerifier,
	}
	got, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("confirm descriptor drift: %v", err)
	}
	if interactiveCredentials.calls != 1 || interactiveVerifier.calls != 1 {
		t.Fatalf("interactive credentials=%d verifier=%d", interactiveCredentials.calls, interactiveVerifier.calls)
	}
	if got.Workspace != "sample" || got.ConfiguredAt.IsZero() {
		t.Fatalf("updated connection = %#v", got)
	}
	if !strings.Contains(out.String(), "change to the configured workspace connection") ||
		!strings.Contains(out.String(), "tenant") ||
		!strings.Contains(out.String(), "Pin workspace") {
		t.Fatalf("interactive drift review output:\n%s", out.String())
	}
}

func TestManagerRejectsResourceChangeAcrossConnectionKeyNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, DescriptorRelativePath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(
		string(raw),
		"/subscriptions/00000000-0000-0000-0000-000000000000/",
		"/subscriptions/22222222-2222-2222-2222-222222222222/",
		1,
	))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "AKS resource") {
		t.Fatalf("expected changed resource to require review, got %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("changed resource fetched credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
}

func TestManagerRejectsNotReadyDuringNoninteractiveRefresh(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Degraded", WorkspaceRevision: "8",
		}},
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), `workspace "sample" is not Ready (phase=Degraded)`) {
		t.Fatalf("expected NotReady failure, got %v", err)
	}
}

func TestManagerRejectsLiveContractChangeNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
			WorkspaceUID: "replacement-uid", WorkspacePhase: "Ready", WorkspaceRevision: "1",
		}},
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "workspace UID") ||
		!strings.Contains(err.Error(), "namespace") ||
		!strings.Contains(err.Error(), "LocalQueue") {
		t.Fatalf("expected changed live contract to require review, got %v", err)
	}

	var out bytes.Buffer
	interactiveVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
		WorkspaceUID: "replacement-uid", WorkspacePhase: "Ready", WorkspaceRevision: "1",
	}}
	interactive := Manager{
		ConfigDir:    configDir,
		Interactive:  true,
		Input:        strings.NewReader("y\n"),
		Output:       &out,
		Verifier:     interactiveVerifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	got, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("confirm live contract drift: %v", err)
	}
	if got.WorkspaceUID != "replacement-uid" ||
		got.Namespace != "sample-v2" ||
		got.Queue != "priority" ||
		got.ConfiguredAt != now.Add(2*time.Minute) {
		t.Fatalf("updated live contract = %#v", got)
	}
	if !strings.Contains(out.String(), "Pin the updated namespace") {
		t.Fatalf("interactive live drift review output:\n%s", out.String())
	}
}

// blockingVerifier models kubelogin waiting on a sign-in prompt that will never
// be answered because no terminal is attached.
type blockingVerifier struct{ calls int }

func (b *blockingVerifier) Verify(ctx context.Context, _ Descriptor, _ string) (Verification, error) {
	b.calls++
	<-ctx.Done()
	return Verification{}, ctx.Err()
}

// Revalidation only reads the TauWorkspace and runs `auth can-i`; it asks the
// user nothing, so an automated caller with no TTY must still get through.
func TestManagerRevalidatesStaleConnectionWithoutTTY(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready", WorkspaceRevision: "9",
	}}
	second := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Credentials:  credentials,
		Verifier:     verifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	got, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("non-interactive stale revalidation: %v", err)
	}
	if credentials.calls != 0 {
		t.Fatalf("revalidation refetched credentials %d times", credentials.calls)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier called %d times, want 1", verifier.calls)
	}
	if got.WorkspaceRevision != "9" {
		t.Fatalf("workspace revision = %q, want 9", got.WorkspaceRevision)
	}
}

// The kubeconfig is kubelogin exec auth, so revalidation can block on a sign-in
// prompt. With no terminal that would hang forever; it must fail fast instead.
func TestManagerBoundsNonInteractiveRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	verifier := &blockingVerifier{}
	second := Manager{
		ConfigDir:           configDir,
		Interactive:         false,
		Credentials:         &fakeCredentialProvider{raw: []byte("must-not-be-used")},
		Verifier:            verifier,
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: 50 * time.Millisecond,
	}
	_, err := second.Ensure(context.Background(), root)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected ErrInteractiveRequired, got %v", err)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier called %d times, want 1", verifier.calls)
	}
}

// An interactive caller must stay unbounded so a real sign-in can complete.
func TestManagerDoesNotBoundInteractiveRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := first.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	second := Manager{
		ConfigDir:           configDir,
		Interactive:         true,
		Credentials:         &fakeCredentialProvider{raw: []byte("must-not-be-used")},
		Verifier:            &blockingVerifier{},
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: time.Millisecond,
	}
	_, err := second.Ensure(ctx, root)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	// It stopped because the caller cancelled, not because a timeout fired.
	if errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("interactive revalidation must not be bounded by the timeout: %v", err)
	}
}

// seedConnection establishes one cached connection interactively so skew tests
// start from a file this binary actually wrote.
func seedConnection(t *testing.T, root, configDir string, now time.Time) {
	t.Helper()
	seed := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := seed.Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}

// Approval must not be mistaken for a terminal: kubelogin can still block on a
// stale credential, so revalidation stays bounded even when stdin has an answer.
func TestManagerPipedApprovalStillBoundsRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	seedConnection(t, root, configDir, now)

	manager := Manager{
		ConfigDir:           configDir,
		Interactive:         false,
		Input:               strings.NewReader("y\n"),
		Credentials:         &fakeCredentialProvider{raw: []byte("unused")},
		Verifier:            &blockingVerifier{},
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: 50 * time.Millisecond,
	}
	if _, err := manager.Ensure(context.Background(), root); !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected bounded revalidation failure, got %v", err)
	}
}

func TestManagerSeparatesClustersWithSameContextName(t *testing.T) {
	firstRoot := writeDescriptorFixture(t)
	secondRoot := t.TempDir()
	secondPath := filepath.Join(secondRoot, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o755); err != nil {
		t.Fatal(err)
	}
	secondRaw := strings.Replace(
		validDescriptorYAML,
		"/subscriptions/00000000-0000-0000-0000-000000000000/",
		"/subscriptions/22222222-2222-2222-2222-222222222222/",
		1,
	)
	if err := os.WriteFile(secondPath, []byte(secondRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	newManager := func() Manager {
		return Manager{
			ConfigDir:   configDir,
			Interactive: false,
			Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
			Verifier: &fakeVerifier{result: Verification{
				ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
				WorkspacePhase: "Ready", WorkspaceRevision: "7",
			}},
		}
	}
	first, err := newManager().Ensure(context.Background(), firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newManager().Ensure(context.Background(), secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.KubeconfigPath == second.KubeconfigPath {
		t.Fatalf("different AKS resource IDs shared kubeconfig path %s", first.KubeconfigPath)
	}
	if _, err := os.Stat(first.KubeconfigPath); err != nil {
		t.Fatalf("first kubeconfig was overwritten or removed: %v", err)
	}
	if _, err := os.Stat(second.KubeconfigPath); err != nil {
		t.Fatalf("second kubeconfig missing: %v", err)
	}
}

func TestManagerEnsureDiscoveryUsesExactDescriptor(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "tau", "workspace.connection.yaml")
	secondPath := filepath.Join(root, "connections", "selected.yaml")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstPath, []byte(strings.Replace(validDescriptorYAML, "workspace: sample", "workspace: wrong", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	discovery, err := LoadFile(secondPath, root)
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready", WorkspaceRevision: "7",
		}},
	}
	connection, err := manager.EnsureDiscovery(context.Background(), discovery)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Workspace != "sample" || connection.DescriptorPath != secondPath {
		t.Fatalf("connection = %#v", connection)
	}
}

// Device code is the supported non-TTY sign-in flow: it prints a code and waits
// for a human to finish in a browser, which routinely outlives any short
// deadline. Bounding credential acquisition would cancel exactly the path this
// command advertises, so it must stay on the caller context.
func TestManagerDoesNotBoundHumanPacedSignIn(t *testing.T) {
	root := writeDescriptorFixture(t)
	credentials := &slowCredentialProvider{delay: 80 * time.Millisecond, raw: []byte("apiVersion: v1\nkind: Config\n")}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: false,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: credentials,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspaceUID: "11111111-1111-1111-1111-111111111111", WorkspacePhase: "Ready",
		}},
		RevalidationTimeout: 10 * time.Millisecond,
	}
	got, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("sign-in slower than the verify deadline must still succeed: %v", err)
	}
	if got.Namespace != "sample" {
		t.Fatalf("namespace = %q, want sample", got.Namespace)
	}
}

// The first verify runs against a kubeconfig whose exec credential may still be
// uncached, so it needs the same bound as the credential fetch.
func TestManagerBoundsBlockingVerifyOnColdStart(t *testing.T) {
	root := writeDescriptorFixture(t)
	verifier := &blockingVerifier{}
	manager := Manager{
		ConfigDir:           t.TempDir(),
		Interactive:         false,
		Input:               strings.NewReader("y\n"),
		Output:              &bytes.Buffer{},
		Credentials:         &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier:            verifier,
		RevalidationTimeout: 50 * time.Millisecond,
	}
	_, err := manager.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected bounded verify, got %v", err)
	}
	if !strings.Contains(err.Error(), "verifying the new workspace connection") {
		t.Fatalf("error must name the stage that blocked, got %q", err.Error())
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier called %d times, want 1", verifier.calls)
	}
}

// An interactive caller must keep the unbounded context: a human can actually
// answer the sign-in prompt, and a 30s cap would abort them mid-browser.
func TestManagerDoesNotBoundInteractiveColdStart(t *testing.T) {
	root := writeDescriptorFixture(t)
	slow := &slowCredentialProvider{delay: 80 * time.Millisecond, raw: []byte("apiVersion: v1\nkind: Config\n")}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: slow,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspaceUID: "11111111-1111-1111-1111-111111111111", WorkspacePhase: "Ready",
		}},
		RevalidationTimeout: 10 * time.Millisecond,
	}
	got, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("interactive cold start must not be bounded: %v", err)
	}
	if got.Namespace != "sample" {
		t.Fatalf("namespace = %q, want sample", got.Namespace)
	}
}

// slowCredentialProvider outlives the non-interactive timeout without blocking
// forever, so it can prove the interactive path is not bounded.
type slowCredentialProvider struct {
	delay time.Duration
	raw   []byte
}

func (s *slowCredentialProvider) UserKubeconfig(ctx context.Context, _ Descriptor) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return append([]byte(nil), s.raw...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// BlobFuse and other fixed-mode filesystems reject chmod on a file this process
// just created. os.CreateTemp already made it 0600, so that must not abort the
// connection, exactly as fileutil.WriteFileAtomic treats it.
func TestWriteKubeconfigCandidateToleratesUnsupportedChmod(t *testing.T) {
	original := candidateChmod
	t.Cleanup(func() { candidateChmod = original })
	candidateChmod = func(*os.File, os.FileMode) error {
		return &os.PathError{Op: "chmod", Err: syscall.EPERM}
	}

	path, err := writeKubeconfigCandidate(t.TempDir(), []byte("apiVersion: v1\n"))
	if err != nil {
		t.Fatalf("unsupported chmod must not fail the candidate write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "apiVersion: v1\n" {
		t.Fatalf("candidate contents = %q", raw)
	}
}

func TestWriteKubeconfigCandidateFailsOnRealChmodError(t *testing.T) {
	original := candidateChmod
	t.Cleanup(func() { candidateChmod = original })
	candidateChmod = func(*os.File, os.FileMode) error {
		return &os.PathError{Op: "chmod", Err: syscall.EIO}
	}

	dir := t.TempDir()
	if _, err := writeKubeconfigCandidate(dir, []byte("apiVersion: v1\n")); err == nil {
		t.Fatal("expected a genuine chmod failure to abort the candidate write")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "kubeconfigs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed candidate left %d file(s) behind", len(entries))
	}
}
