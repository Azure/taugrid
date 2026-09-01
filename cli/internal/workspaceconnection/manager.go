// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/fileutil"
)

const (
	connectionStateSchema = "tau.workspace.connection-state.v1"
	// connectionStateSchemaV2 is written by newer Tau clients. The routing
	// fields consumed by ListCachedConnections are unchanged.
	connectionStateSchemaV2 = "tau.workspace.connection-state.v2"
)

// defaultRevalidationTimeout bounds non-interactive revalidation. Overridable
// per-Manager for tests; there is no env knob until someone needs one.
const defaultRevalidationTimeout = 30 * time.Second

var (
	ErrInteractiveRequired = errors.New("workspace connection requires interactive review")
	ErrConnectionDeclined  = errors.New("workspace connection change was not confirmed")
)

type CredentialProvider interface {
	UserKubeconfig(context.Context, Descriptor) ([]byte, error)
}

type Verifier interface {
	Verify(context.Context, Descriptor, string) (Verification, error)
}

type Verification struct {
	ContextName    string
	Namespace      string
	Queue          string
	ServiceAccount string
	WorkspaceUID   string
	WorkspacePhase string
}

type ActiveConnection struct {
	Workspace         string
	WorkspaceUID      string
	AuthorizationMode string
	ContextName       string
	SystemNamespace   string
	KubeconfigPath    string
	Namespace         string
	Queue             string
}

type connectionState struct {
	Schema            string       `json:"schema"`
	Workspace         string       `json:"workspace"`
	AccessMethod      AccessMethod `json:"access_method"`
	AccessIdentity    string       `json:"access_identity"`
	AccessFingerprint string       `json:"access_fingerprint,omitempty"`
	AuthorizationMode string       `json:"authorization_mode"`
	ContextName       string       `json:"context_name"`
	SystemNamespace   string       `json:"system_namespace,omitempty"`
	KubeconfigPath    string       `json:"kubeconfig_path"`
	Namespace         string       `json:"namespace"`
	Queue             string       `json:"queue"`
	ServiceAccount    string       `json:"service_account,omitempty"`
	RequiredRole      string       `json:"required_role"`
	DescriptorPath    string       `json:"descriptor_path"`
	DescriptorDigest  string       `json:"descriptor_digest"`
	WorkspaceUID      string       `json:"workspace_uid,omitempty"`
	ConfiguredAt      time.Time    `json:"configured_at,omitempty"`
	VerifiedAt        time.Time    `json:"verified_at"`
}

type Manager struct {
	ConfigDir string
	// Interactive reports whether stdin is a terminal. It no longer gates
	// consent — the confirmation prompt reads piped stdin like any other unix
	// tool — and now only bounds credential revalidation, which may need a
	// terminal of its own for kubelogin.
	Interactive  bool
	Input        io.Reader
	Output       io.Writer
	Credentials  CredentialProvider
	Verifier     Verifier
	Now          func() time.Time
	ReadinessTTL time.Duration
	// RevalidationTimeout bounds non-interactive revalidation. Zero falls back
	// to TAU_CONNECTION_REVALIDATION_TIMEOUT, then defaultRevalidationTimeout.
	RevalidationTimeout time.Duration
	TauVersion          string
}

func DefaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "tau"), nil
}

// ListCachedConnections returns the verified workspace routes Tau has already
// configured locally. It does not refresh credentials or contact a cluster.
func ListCachedConnections(configDir string) ([]ActiveConnection, error) {
	if strings.TrimSpace(configDir) == "" {
		var err error
		configDir, err = DefaultConfigDir()
		if err != nil {
			return nil, err
		}
	}
	connectionsDir := filepath.Join(filepath.Clean(configDir), "connections")
	entries, err := os.ReadDir(connectionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect cached Tau workspace connections: %w", err)
	}

	connections := make([]ActiveConnection, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(connectionsDir, entry.Name())
		state, err := loadConnectionState(path)
		if err != nil {
			return nil, err
		}
		if state.Schema != connectionStateSchema && state.Schema != connectionStateSchemaV2 {
			return nil, fmt.Errorf("cached Tau workspace connection %s uses unsupported schema %q", path, state.Schema)
		}
		if state.ConfiguredAt.IsZero() || strings.TrimSpace(state.WorkspaceUID) == "" {
			continue
		}
		connection := state.active()
		if strings.TrimSpace(connection.Workspace) == "" ||
			strings.TrimSpace(connection.ContextName) == "" ||
			strings.TrimSpace(connection.KubeconfigPath) == "" ||
			strings.TrimSpace(connection.Namespace) == "" {
			continue
		}
		connections = append(connections, connection)
	}
	sort.Slice(connections, func(i, j int) bool {
		left := connections[i]
		right := connections[j]
		if left.Workspace != right.Workspace {
			return left.Workspace < right.Workspace
		}
		if left.ContextName != right.ContextName {
			return left.ContextName < right.ContextName
		}
		return left.Namespace < right.Namespace
	})
	return connections, nil
}

func (m Manager) Ensure(ctx context.Context, startDir string) (ActiveConnection, error) {
	discovery, err := Discover(startDir)
	if err != nil {
		return ActiveConnection{}, err
	}
	return m.EnsureDiscovery(ctx, discovery)
}

// EnsureDiscovery activates one already-resolved descriptor. Callers use this
// for catalog entries so connection routing cannot fall back to ancestor search.
func (m Manager) EnsureDiscovery(ctx context.Context, discovery Discovery) (ActiveConnection, error) {
	if strings.TrimSpace(m.TauVersion) != "" {
		if err := CheckTauVersion(m.TauVersion, discovery.Descriptor.Requirements.MinTauVersion); err != nil {
			return ActiveConnection{}, err
		}
	}
	configDir, err := m.configDir()
	if err != nil {
		return ActiveConnection{}, err
	}
	connectionKey := ConnectionKeyForDiscovery(discovery)
	statePath := filepath.Join(configDir, "connections", connectionKey+".json")
	state, loadedStatePath, stateErr := loadConnectionStateForDiscovery(statePath, discovery)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return ActiveConnection{}, fmt.Errorf("load Tau workspace connection state: %w", stateErr)
	}
	hasState := stateErr == nil &&
		(loadedStatePath == statePath || state.DescriptorPath == discovery.Path)
	stateConfigured := hasState && state.configures(discovery)
	kubeconfigMissing := stateConfigured && !fileExists(state.KubeconfigPath)
	stateMatches := stateConfigured && !kubeconfigMissing
	var sourceKubeconfig []byte
	var sourceFingerprint string
	sourceKubeconfigChanged := false
	sourceTargetChanged := false
	if hasState &&
		state.AccessMethod == discovery.Descriptor.Access.Method &&
		discovery.Descriptor.tracksKubeconfigSource() {
		sourceKubeconfig, sourceFingerprint, err = m.resolveUserKubeconfig(ctx, discovery.Descriptor, nil)
		if err != nil {
			return ActiveConnection{}, fmt.Errorf("refresh kubeconfig workspace access: %w", err)
		}
		sourceTargetChanged = state.AccessFingerprint != sourceFingerprint
		if sourceTargetChanged {
			stateMatches = false
			kubeconfigMissing = false
		} else if stateMatches {
			installedKubeconfig, err := os.ReadFile(state.KubeconfigPath)
			if err != nil {
				return ActiveConnection{}, fmt.Errorf("read isolated Tau kubeconfig: %w", err)
			}
			sourceKubeconfigChanged = !bytes.Equal(installedKubeconfig, sourceKubeconfig)
		}
	}
	if stateMatches && m.stateFresh(state) && !sourceKubeconfigChanged {
		return state.active(), nil
	}
	if stateMatches {
		if m.Verifier == nil {
			return ActiveConnection{}, fmt.Errorf("workspace connection verifier is not configured")
		}
		// Revalidation reads the TauWorkspace and runs `auth can-i` checks; it
		// never asks the user anything, so it must not require a terminal.
		// A kubeconfig exec plugin can block on browser or device-code sign-in
		// when its token cache is cold. Without a terminal that would hang
		// forever, which is worse for an automated caller than a clean failure,
		// so bound it and report how to recover.
		verifyCtx, cancel := m.signInDeadline(ctx)
		defer cancel()
		var verification Verification
		var verifyErr error
		if len(sourceKubeconfig) > 0 {
			verification, verifyErr = m.verifyCandidate(verifyCtx, configDir, discovery, sourceKubeconfig)
		} else {
			verification, verifyErr = m.Verifier.Verify(verifyCtx, discovery.Descriptor, state.KubeconfigPath)
		}
		if verifyErr != nil {
			if timeout := m.signInTimeout(verifyCtx, "revalidating the cached workspace connection"); timeout != nil {
				return ActiveConnection{}, timeout
			}
			return ActiveConnection{}, fmt.Errorf("revalidate Tau workspace connection: %w", verifyErr)
		}
		if err := validateVerification(discovery.Descriptor, verification); err != nil {
			return ActiveConnection{}, fmt.Errorf("revalidate Tau workspace connection: %w", err)
		}
		if changes := state.contractChanges(verification); len(changes) > 0 {
			if !m.Interactive {
				return ActiveConnection{}, fmt.Errorf(
					"%w: cached workspace connection contract changed (%s)",
					ErrInteractiveRequired,
					strings.Join(changes, ", "),
				)
			}
			confirmed, err := m.confirmContractChange(state, verification, changes)
			if err != nil {
				return ActiveConnection{}, err
			}
			if !confirmed {
				return ActiveConnection{}, ErrConnectionDeclined
			}
			state.ConfiguredAt = m.now()
		}
		if len(sourceKubeconfig) > 0 {
			if err := fileutil.WriteFileAtomic(state.KubeconfigPath, sourceKubeconfig, 0o600); err != nil {
				return ActiveConnection{}, fmt.Errorf("refresh isolated Tau kubeconfig: %w", err)
			}
			state.AccessFingerprint = sourceFingerprint
		}
		state.applyVerification(discovery.Descriptor, verification, m.now())
		if err := fileutil.WriteJSONFileAtomic(loadedStatePath, state); err != nil {
			return ActiveConnection{}, fmt.Errorf("update Tau workspace connection state: %w", err)
		}
		return state.active(), nil
	}
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return ActiveConnection{}, stateErr
	}
	if kubeconfigMissing {
		if m.Verifier == nil {
			return ActiveConnection{}, fmt.Errorf("workspace connection verifier is not configured")
		}
		rawKubeconfig, accessFingerprint, err := m.resolveUserKubeconfig(ctx, discovery.Descriptor, sourceKubeconfig)
		if err != nil {
			return ActiveConnection{}, fmt.Errorf("reacquire Kubernetes credentials: %w", err)
		}
		kubeconfigPath := isolatedKubeconfigPath(configDir, discovery)
		verification, err := m.verifyCandidate(ctx, configDir, discovery, rawKubeconfig)
		if err != nil {
			return ActiveConnection{}, fmt.Errorf("verify reacquired Tau workspace connection: %w", err)
		}
		if changes := state.contractChanges(verification); len(changes) > 0 {
			if !m.Interactive {
				return ActiveConnection{}, fmt.Errorf(
					"%w: cached workspace connection contract changed (%s)",
					ErrInteractiveRequired,
					strings.Join(changes, ", "),
				)
			}
			confirmed, err := m.confirmContractChange(state, verification, changes)
			if err != nil {
				return ActiveConnection{}, err
			}
			if !confirmed {
				return ActiveConnection{}, ErrConnectionDeclined
			}
			state.ConfiguredAt = m.now()
		}
		finalPreexisted := fileExists(kubeconfigPath)
		if err := fileutil.WriteFileAtomic(kubeconfigPath, rawKubeconfig, 0o600); err != nil {
			return ActiveConnection{}, fmt.Errorf("replace isolated Tau kubeconfig: %w", err)
		}
		state.AccessFingerprint = accessFingerprint
		state.KubeconfigPath = kubeconfigPath
		state.applyVerification(discovery.Descriptor, verification, m.now())
		if err := fileutil.WriteJSONFileAtomic(loadedStatePath, state); err != nil {
			if !finalPreexisted {
				_ = os.Remove(kubeconfigPath)
			}
			return ActiveConnection{}, fmt.Errorf("update Tau workspace connection state: %w", err)
		}
		return state.active(), nil
	}
	if hasState {
		changes := state.configurationChanges(discovery)
		if sourceTargetChanged {
			changes = append(changes, "Kubernetes context target changed")
		}
		if !m.Interactive {
			return ActiveConnection{}, fmt.Errorf(
				"%w: configured workspace connection changed (%s)",
				ErrInteractiveRequired,
				strings.Join(changes, ", "),
			)
		}
		confirmed, err := m.confirmConfigurationChange(state, discovery.Descriptor, changes)
		if err != nil {
			return ActiveConnection{}, err
		}
		if !confirmed {
			return ActiveConnection{}, ErrConnectionDeclined
		}
	}
	if !hasState {
		if !m.Interactive {
			return ActiveConnection{}, fmt.Errorf(
				"%w: this repository has not been connected with Tau on this machine; run `tau workspace connection` from an interactive terminal to review and approve the destination",
				ErrInteractiveRequired,
			)
		}
		confirmed, err := m.confirmFirstUse(discovery)
		if err != nil {
			return ActiveConnection{}, err
		}
		if !confirmed {
			return ActiveConnection{}, ErrConnectionDeclined
		}
	}
	if m.Verifier == nil {
		return ActiveConnection{}, fmt.Errorf("workspace connection verifier is not configured")
	}

	// Credential acquisition stays on the caller context. Provider-backed access
	// may require a human-paced sign-in, while kubeconfig access may invoke its
	// own exec credential plugin.
	rawKubeconfig, accessFingerprint, err := m.resolveUserKubeconfig(ctx, discovery.Descriptor, sourceKubeconfig)
	if err != nil {
		return ActiveConnection{}, fmt.Errorf("obtain Kubernetes credentials: %w", err)
	}
	kubeconfigPath := isolatedKubeconfigPath(configDir, discovery)
	verification, err := m.verifyCandidate(ctx, configDir, discovery, rawKubeconfig)
	if err != nil {
		return ActiveConnection{}, fmt.Errorf("verify Tau workspace connection: %w", err)
	}
	if hasState {
		if changes := state.contractChanges(verification); len(changes) > 0 {
			confirmed, err := m.confirmContractChange(state, verification, changes)
			if err != nil {
				return ActiveConnection{}, err
			}
			if !confirmed {
				return ActiveConnection{}, ErrConnectionDeclined
			}
		}
	}
	finalPreexisted := fileExists(kubeconfigPath)
	if err := fileutil.WriteFileAtomic(kubeconfigPath, rawKubeconfig, 0o600); err != nil {
		return ActiveConnection{}, fmt.Errorf("write isolated Tau kubeconfig: %w", err)
	}
	previousKubeconfigPath := state.KubeconfigPath
	now := m.now()
	state = connectionState{
		Schema:            connectionStateSchema,
		Workspace:         discovery.Descriptor.Workspace,
		AccessMethod:      discovery.Descriptor.Access.Method,
		AccessIdentity:    discovery.Descriptor.AccessIdentity(),
		AccessFingerprint: accessFingerprint,
		AuthorizationMode: discovery.Descriptor.Authorization.Mode,
		SystemNamespace:   discovery.Descriptor.ResolvedSystemNamespace(),
		KubeconfigPath:    kubeconfigPath,
		RequiredRole:      discovery.Descriptor.Authorization.RequiredRole,
		DescriptorPath:    discovery.Path,
		DescriptorDigest:  discovery.Digest,
		ConfiguredAt:      now,
	}
	state.applyVerification(discovery.Descriptor, verification, now)
	if err := fileutil.WriteJSONFileAtomic(statePath, state); err != nil {
		if !finalPreexisted {
			_ = os.Remove(kubeconfigPath)
		}
		return ActiveConnection{}, fmt.Errorf("write Tau workspace connection state: %w", err)
	}
	if hasState {
		if loadedStatePath != statePath {
			_ = os.Remove(loadedStatePath)
		}
		if previousKubeconfigPath != kubeconfigPath {
			_ = os.Remove(previousKubeconfigPath)
		}
	}
	return state.active(), nil
}

func (m Manager) configDir() (string, error) {
	if strings.TrimSpace(m.ConfigDir) != "" {
		return filepath.Clean(m.ConfigDir), nil
	}
	return DefaultConfigDir()
}

func (m Manager) confirmContractChange(state connectionState, verification Verification, changes []string) (bool, error) {
	output := m.Output
	if output == nil {
		output = os.Stdout
	}
	fmt.Fprintln(output, "Tau detected a change to the configured workspace connection contract:")
	for _, change := range changes {
		fmt.Fprintf(output, "  - %s\n", change)
	}
	return m.readConfirmation(
		fmt.Sprintf(
			"\nPin the updated namespace %q and LocalQueue %q for workspace %q? [y/N] ",
			verification.Namespace,
			verification.Queue,
			state.Workspace,
		),
		"workspace connection contract",
	)
}

func (m Manager) confirmConfigurationChange(state connectionState, descriptor Descriptor, changes []string) (bool, error) {
	output := m.Output
	if output == nil {
		output = os.Stdout
	}
	fmt.Fprintln(output, "Tau detected a change to the configured workspace connection:")
	for _, change := range changes {
		fmt.Fprintf(output, "  - %s\n", change)
	}
	return m.readConfirmation(
		fmt.Sprintf(
			"\nPin workspace %q on Kubernetes context %q for this repository? [y/N] ",
			descriptor.Workspace,
			descriptor.Cluster.ContextName,
		),
		"workspace connection configuration",
	)
}

func (m Manager) confirmFirstUse(discovery Discovery) (bool, error) {
	output := m.Output
	if output == nil {
		output = os.Stdout
	}
	descriptor := discovery.Descriptor
	fmt.Fprintln(output, "First-time workspace connection")
	fmt.Fprintln(output, "This repository has not been connected with Tau on this machine.")
	fmt.Fprintln(output, "Review where Tau will connect:")
	fmt.Fprintf(output, "  Descriptor:      %s\n", discovery.Path)
	fmt.Fprintf(output, "  Workspace:       %s\n", descriptor.Workspace)
	fmt.Fprintf(output, "  Access method:   %s\n", descriptor.Access.Method)
	fmt.Fprintf(output, "  Context:         %s\n", descriptor.Cluster.ContextName)
	fmt.Fprintf(output, "  Authorization:   %s\n", descriptor.Authorization.Mode)
	if descriptor.Network.PrivateCluster {
		fmt.Fprintln(output, "  Private network: required")
	} else {
		fmt.Fprintln(output, "  Private network: not indicated")
	}
	if descriptor.Access.AKS != nil {
		fmt.Fprintf(output, "  AKS resource:    %s\n", descriptor.Access.AKS.ResourceID)
		fmt.Fprintf(output, "  Entra tenant:    %s\n", descriptor.Access.AKS.TenantID)
	}
	fmt.Fprintln(output, "\nIf approved, Tau will acquire your credentials, verify this workspace,")
	fmt.Fprintln(output, "and save an isolated local connection for future commands.")
	fmt.Fprintln(output, "Nothing has been accessed or saved yet.")
	return m.readConfirmation(
		"\nApprove and connect? [y/N] ",
		"first workspace connection",
	)
}

func (m Manager) readConfirmation(prompt, description string) (bool, error) {
	input := m.Input
	if input == nil {
		input = os.Stdin
	}
	output := m.Output
	if output == nil {
		output = os.Stdout
	}
	fmt.Fprint(output, prompt)
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read %s confirmation: %w", description, err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "", "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s confirmation must be yes or no", description)
	}
}

func (m Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m Manager) stateFresh(state connectionState) bool {
	ttl := m.ReadinessTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	if ttl < 0 {
		return false
	}
	return !state.VerifiedAt.IsZero() && m.now().Sub(state.VerifiedAt) <= ttl
}

func validateVerification(descriptor Descriptor, verification Verification) error {
	contextName := firstNonEmpty(verification.ContextName, descriptor.Cluster.ContextName)
	if contextName != descriptor.Cluster.ContextName {
		return fmt.Errorf(
			"workspace %q resolved cluster context %q, expected %q",
			descriptor.Workspace,
			contextName,
			descriptor.Cluster.ContextName,
		)
	}
	if verification.WorkspacePhase != "Ready" {
		return fmt.Errorf(
			"workspace %q is not Ready (phase=%s)",
			descriptor.Workspace,
			verification.WorkspacePhase,
		)
	}
	if strings.TrimSpace(verification.WorkspaceUID) == "" {
		return fmt.Errorf("workspace %q has no stable Kubernetes UID", descriptor.Workspace)
	}
	if strings.TrimSpace(verification.Namespace) == "" {
		return fmt.Errorf("workspace %q has no resolved target namespace", descriptor.Workspace)
	}
	if strings.TrimSpace(verification.Queue) == "" {
		return fmt.Errorf("workspace %q has no resolved LocalQueue", descriptor.Workspace)
	}
	return nil
}

// signInDeadline bounds a call that may block on a credential-plugin prompt.
// Without a terminal that prompt can never be answered, so hanging forever is
// strictly worse for an automated caller than failing with instructions.
// Interactive callers keep the unbounded context so a human can complete it.
func (m Manager) signInDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.Interactive {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.revalidationTimeout())
}

// signInTimeout returns the actionable error for a call bounded by
// signInDeadline, or nil when the failure was not the deadline.
func (m Manager) signInTimeout(bounded context.Context, stage string) error {
	if m.Interactive || !errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf(
		"%w: %s timed out after %s, most likely because the Kubernetes credential provider needed a sign-in prompt and no terminal is attached; run a tau command in a terminal once to complete sign-in",
		ErrInteractiveRequired,
		stage,
		m.revalidationTimeout(),
	)
}

func (m Manager) revalidationTimeout() time.Duration {
	if m.RevalidationTimeout > 0 {
		return m.RevalidationTimeout
	}
	return defaultRevalidationTimeout
}

func loadConnectionState(path string) (connectionState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return connectionState{}, err
	}
	var state connectionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return connectionState{}, fmt.Errorf("parse Tau workspace connection state %s: %w", path, err)
	}
	return state, nil
}

func loadConnectionStateForDiscovery(statePath string, discovery Discovery) (connectionState, string, error) {
	state, err := loadConnectionState(statePath)
	if err == nil {
		return state, statePath, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return connectionState{}, "", err
	}
	connectionsDir := filepath.Dir(statePath)
	entries, readErr := os.ReadDir(connectionsDir)
	if errors.Is(readErr, os.ErrNotExist) {
		return connectionState{}, "", os.ErrNotExist
	}
	if readErr != nil {
		return connectionState{}, "", fmt.Errorf("inspect Tau workspace connection states: %w", readErr)
	}
	var matchedState connectionState
	matchedPath := ""
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		candidatePath := filepath.Join(connectionsDir, entry.Name())
		candidate, candidateErr := loadConnectionState(candidatePath)
		if candidateErr != nil || candidate.DescriptorPath != discovery.Path {
			continue
		}
		if matchedPath != "" {
			return connectionState{}, "", fmt.Errorf(
				"multiple Tau workspace connection states pin descriptor %s",
				discovery.Path,
			)
		}
		matchedState = candidate
		matchedPath = candidatePath
	}
	if matchedPath == "" {
		return connectionState{}, "", os.ErrNotExist
	}
	return matchedState, matchedPath, nil
}

func (s connectionState) configures(discovery Discovery) bool {
	supportedSchema := s.Schema == connectionStateSchema
	hasConfiguration := !s.ConfiguredAt.IsZero()
	hasStableWorkspaceIdentity := strings.TrimSpace(s.WorkspaceUID) != ""
	hasTrustIdentity := s.AccessMethod == discovery.Descriptor.Access.Method &&
		s.AccessIdentity == discovery.Descriptor.AccessIdentity()
	return supportedSchema &&
		hasConfiguration &&
		hasStableWorkspaceIdentity &&
		hasTrustIdentity &&
		s.Workspace == discovery.Descriptor.Workspace &&
		s.AuthorizationMode == discovery.Descriptor.Authorization.Mode &&
		s.RequiredRole == discovery.Descriptor.Authorization.RequiredRole &&
		s.ContextName == discovery.Descriptor.Cluster.ContextName &&
		s.DescriptorPath == discovery.Path &&
		s.DescriptorDigest == discovery.Digest
}

func (s connectionState) configurationChanges(discovery Discovery) []string {
	var changes []string
	if s.Schema != connectionStateSchema {
		changes = append(changes, fmt.Sprintf("state schema %q is unsupported", s.Schema))
	}
	if s.Workspace != discovery.Descriptor.Workspace {
		changes = append(changes, fmt.Sprintf("workspace %q -> %q", s.Workspace, discovery.Descriptor.Workspace))
	}
	if s.AccessMethod != discovery.Descriptor.Access.Method {
		changes = append(changes, fmt.Sprintf("access method %q -> %q", s.AccessMethod, discovery.Descriptor.Access.Method))
	}
	if s.AccessIdentity != discovery.Descriptor.AccessIdentity() {
		changes = append(changes, "access identity changed")
	}
	if s.ContextName != discovery.Descriptor.Cluster.ContextName {
		changes = append(changes, fmt.Sprintf("Kubernetes context %q -> %q", s.ContextName, discovery.Descriptor.Cluster.ContextName))
	}
	if s.SystemNamespace != discovery.Descriptor.ResolvedSystemNamespace() {
		changes = append(changes, fmt.Sprintf("system namespace %q -> %q", s.SystemNamespace, discovery.Descriptor.ResolvedSystemNamespace()))
	}
	if s.AuthorizationMode != discovery.Descriptor.Authorization.Mode {
		changes = append(changes, fmt.Sprintf("authorization mode %q -> %q", s.AuthorizationMode, discovery.Descriptor.Authorization.Mode))
	}
	if s.RequiredRole != discovery.Descriptor.Authorization.RequiredRole {
		changes = append(changes, fmt.Sprintf("required role %q -> %q", s.RequiredRole, discovery.Descriptor.Authorization.RequiredRole))
	}
	if s.DescriptorDigest != discovery.Digest {
		changes = append(changes, fmt.Sprintf("descriptor digest %q -> %q", s.DescriptorDigest, discovery.Digest))
	}
	if s.Schema == connectionStateSchema && s.ConfiguredAt.IsZero() {
		changes = append(changes, "configuration timestamp is missing")
	}
	if s.Schema == connectionStateSchema && strings.TrimSpace(s.WorkspaceUID) == "" {
		changes = append(changes, "workspace UID pin is missing")
	}
	return changes
}

func (s connectionState) contractChanges(verification Verification) []string {
	var changes []string
	if s.WorkspaceUID != "" && s.WorkspaceUID != verification.WorkspaceUID {
		changes = append(changes, fmt.Sprintf("workspace UID %q -> %q", s.WorkspaceUID, verification.WorkspaceUID))
	}
	if s.Namespace != verification.Namespace {
		changes = append(changes, fmt.Sprintf("namespace %q -> %q", s.Namespace, verification.Namespace))
	}
	if s.Queue != verification.Queue {
		changes = append(changes, fmt.Sprintf("LocalQueue %q -> %q", s.Queue, verification.Queue))
	}
	storedServiceAccount := effectiveServiceAccount(s.ServiceAccount)
	verifiedServiceAccount := effectiveServiceAccount(verification.ServiceAccount)
	if storedServiceAccount != verifiedServiceAccount {
		changes = append(changes, fmt.Sprintf("service account %q -> %q", storedServiceAccount, verifiedServiceAccount))
	}
	return changes
}

func (s *connectionState) applyVerification(descriptor Descriptor, verification Verification, verifiedAt time.Time) {
	s.ContextName = firstNonEmpty(verification.ContextName, descriptor.Cluster.ContextName)
	s.Namespace = verification.Namespace
	s.Queue = verification.Queue
	s.ServiceAccount = verification.ServiceAccount
	s.WorkspaceUID = verification.WorkspaceUID
	s.VerifiedAt = verifiedAt
}

func effectiveServiceAccount(name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return "default"
}

func (s connectionState) active() ActiveConnection {
	return ActiveConnection{
		Workspace:         s.Workspace,
		WorkspaceUID:      s.WorkspaceUID,
		AuthorizationMode: s.AuthorizationMode,
		ContextName:       s.ContextName,
		SystemNamespace:   firstNonEmpty(s.SystemNamespace, tauworkspace.SystemNamespace),
		KubeconfigPath:    s.KubeconfigPath,
		Namespace:         s.Namespace,
		Queue:             s.Queue,
	}
}

func safeFilename(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "connection"
	}
	return name
}

func ConnectionKey(descriptor Descriptor) string {
	return safeFilename(descriptor.Workspace) + "-" + accessIdentityHash(descriptor.AccessIdentity())
}

func ConnectionKeyForDiscovery(discovery Discovery) string {
	return ConnectionKey(discovery.Descriptor) + "-" + accessIdentityHash(discovery.Path)
}

func accessIdentityHash(identity string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(identity)))
	return hex.EncodeToString(sum[:6])
}

func descriptorDigestHash(digest string) string {
	value := strings.TrimPrefix(digest, "sha256:")
	if len(value) > 12 {
		return value[:12]
	}
	return safeFilename(value)
}

func descriptorAccessFingerprint(descriptor Descriptor, rawKubeconfig []byte) (string, error) {
	if !descriptor.tracksKubeconfigSource() {
		return "", nil
	}
	fingerprint, err := kubeconfigAccessFingerprint(rawKubeconfig, descriptor.Cluster.ContextName)
	if err != nil {
		return "", fmt.Errorf("inspect kubeconfig workspace access: %w", err)
	}
	return fingerprint, nil
}

func (m Manager) resolveUserKubeconfig(
	ctx context.Context,
	descriptor Descriptor,
	source []byte,
) ([]byte, string, error) {
	raw := source
	if len(raw) == 0 {
		if m.Credentials == nil {
			return nil, "", fmt.Errorf("workspace connection credential provider is not configured")
		}
		var err error
		raw, err = m.Credentials.UserKubeconfig(ctx, descriptor)
		if err != nil {
			return nil, "", err
		}
	}
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("provider returned an empty kubeconfig")
	}
	fingerprint, err := descriptorAccessFingerprint(descriptor, raw)
	if err != nil {
		return nil, "", err
	}
	return raw, fingerprint, nil
}

func kubeconfigAccessFingerprint(rawKubeconfig []byte, contextName string) (string, error) {
	config, err := clientcmd.Load(rawKubeconfig)
	if err != nil {
		return "", fmt.Errorf("parse Kubernetes configuration: %w", err)
	}
	kubeContext, ok := config.Contexts[contextName]
	if !ok {
		return "", fmt.Errorf("Kubernetes context %q is missing", contextName)
	}
	cluster, ok := config.Clusters[kubeContext.Cluster]
	if !ok {
		return "", fmt.Errorf("Kubernetes context %q references missing cluster %q", contextName, kubeContext.Cluster)
	}
	rawCluster, err := json.Marshal(cluster)
	if err != nil {
		return "", fmt.Errorf("encode Kubernetes cluster target: %w", err)
	}
	sum := sha256.Sum256(rawCluster)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func isolatedKubeconfigPath(configDir string, discovery Discovery) string {
	return filepath.Join(
		configDir,
		"kubeconfigs",
		safeFilename(discovery.Descriptor.Cluster.ContextName)+"-"+
			accessIdentityHash(discovery.Descriptor.AccessIdentity())+"-"+
			accessIdentityHash(discovery.Path)+"-"+
			descriptorDigestHash(discovery.Digest)+".yaml",
	)
}

func (m Manager) verifyCandidate(
	ctx context.Context,
	configDir string,
	discovery Discovery,
	rawKubeconfig []byte,
) (Verification, error) {
	candidatePath, err := writeKubeconfigCandidate(configDir, rawKubeconfig)
	if err != nil {
		return Verification{}, err
	}
	defer func() {
		_ = os.Remove(candidatePath)
	}()
	// The candidate kubeconfig is kubelogin exec auth, so this verify can block
	// on a sign-in prompt that a caller with no terminal can never answer.
	verifyCtx, cancel := m.signInDeadline(ctx)
	defer cancel()
	verification, err := m.Verifier.Verify(verifyCtx, discovery.Descriptor, candidatePath)
	if err != nil {
		if timeout := m.signInTimeout(verifyCtx, "verifying the new workspace connection"); timeout != nil {
			return Verification{}, timeout
		}
		return Verification{}, err
	}
	if err := validateVerification(discovery.Descriptor, verification); err != nil {
		return Verification{}, err
	}
	return verification, nil
}

// candidateChmod is overridden in tests to simulate filesystems (BlobFuse and
// friends) that reject chmod on a file this process just created.
var candidateChmod = func(f *os.File, perm os.FileMode) error { return f.Chmod(perm) }

func writeKubeconfigCandidate(configDir string, rawKubeconfig []byte) (string, error) {
	kubeconfigDir := filepath.Join(configDir, "kubeconfigs")
	if err := os.MkdirAll(kubeconfigDir, 0o700); err != nil {
		return "", fmt.Errorf("create Tau kubeconfig directory: %w", err)
	}
	candidate, err := os.CreateTemp(kubeconfigDir, ".candidate-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create Tau kubeconfig candidate: %w", err)
	}
	candidatePath := candidate.Name()
	removeCandidate := func() {
		_ = candidate.Close()
		_ = os.Remove(candidatePath)
	}
	if err := candidateChmod(candidate, 0o600); err != nil && !fileutil.ChmodUnsupported(err) {
		removeCandidate()
		return "", fmt.Errorf("secure Tau kubeconfig candidate: %w", err)
	}
	if _, err := candidate.Write(rawKubeconfig); err != nil {
		removeCandidate()
		return "", fmt.Errorf("write Tau kubeconfig candidate: %w", err)
	}
	if err := candidate.Sync(); err != nil {
		removeCandidate()
		return "", fmt.Errorf("sync Tau kubeconfig candidate: %w", err)
	}
	if err := candidate.Close(); err != nil {
		_ = os.Remove(candidatePath)
		return "", fmt.Errorf("close Tau kubeconfig candidate: %w", err)
	}
	return candidatePath, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
