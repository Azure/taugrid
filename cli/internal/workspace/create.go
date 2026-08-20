// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	DefaultWorkspaceQueue = "jobqueue"
	DefaultResearcherRole = "tau-researcher-v1"

	// Entra asserts groups by object ID, so a subject naming a group that does
	// not exist binds nobody. GitHub asserts team slugs, which share a shape
	// with workspace names, so the same fallback there could bind a real team.
	PrincipalProviderEntra  = "entra"
	PrincipalProviderGitHub = "github"

	SubjectKindGroup          = "Group"
	SubjectKindUser           = "User"
	SubjectKindServiceAccount = "ServiceAccount"

	// DefaultWorkspaceName is the workspace every TauGrid v0 cluster gets
	// unless an operator deliberately picks another name.
	//
	// v0 activates exactly one workspace per cluster, so leaving the name
	// unset made every install differ in the one place researchers could see:
	// the workload namespace, which is derived from this name. Defaulting it
	// means a stock install of TauGrid looks the same everywhere, which is
	// what lets docs, examples, and tooling assume a shape instead of
	// parameterising over an operator's choice.
	//
	// The name is still customisable; nothing may depend on this value being
	// literal. `tau` resolves the active workspace from the cluster rather
	// than assuming this constant.
	//
	// This shares a name with the taugrid-default PriorityClass. That is a
	// different resource kind, so the two never collide.
	DefaultWorkspaceName = workloadmeta.DefaultWorkspaceName
)

type CreateOptions struct {
	Name                     string
	Namespace                string
	SystemNamespace          string
	Queue                    string
	PrincipalProvider        string
	PrincipalName            string
	KubernetesSubjectKind    string
	KubernetesSubjectName    string
	OutputRoot               string
	Priority                 string
	ServiceAccountName       string
	WorkloadIdentityClientID string

	// DefaultPrincipalToName lets an unset --principal-name fall back to the
	// workspace name. The caller sets it from flag presence, because an
	// explicitly-empty value means a shell variable did not expand.
	DefaultPrincipalToName bool
}

type CreateReport struct {
	ClusterQueueUID         string
	ExistingWorkspaceName   string
	ExistingWorkspaceIntent string
}

func (r CreateReport) Summary() string {
	if r.ExistingWorkspaceIntent == "compatible" {
		return fmt.Sprintf(
			"preflight passed: TauWorkspace %q already declares the requested v0 workspace; no changes needed",
			r.ExistingWorkspaceName,
		)
	}
	return fmt.Sprintf(
		"preflight passed: ClusterQueue %q exists (uid=%s); the controller will create the Namespace, researcher RBAC, and LocalQueue",
		DefaultWorkspaceQueue,
		r.ClusterQueueUID,
	)
}

// entraObjectID matches the shape AKS puts in a token's groups claim. Entra
// asserts groups by object ID, so a name of this shape is the one string that
// could make a "nobody asserts this" subject real.
var entraObjectID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// defaultedPrincipalBindsNobody reports whether falling back to the workspace
// name would produce a subject no identity provider can assert. It is the whole
// safety argument for defaulting --principal-name, so it fails closed: every
// combination not proven inert has to name its principal.
//
// Entra Groups are the one proven case, and only for a name that cannot be an
// object ID. The AKS groups claim carries object IDs, so a Group named after a
// workspace matches nothing — unless the workspace was itself named after one,
// which a UUID-shaped name allows because it is a valid DNS-1123 subdomain.
//
// Entra Users assert by UPN, GitHub by team slug, and both share a shape with
// workspace names. A ServiceAccount is worse still: the controller can create
// one in the workspace namespace, so the subject could become real without
// anyone naming it.
func defaultedPrincipalBindsNobody(provider, subjectKind, name string) bool {
	return provider == PrincipalProviderEntra &&
		subjectKind == SubjectKindGroup &&
		!entraObjectID.MatchString(name)
}

// PrincipalWasDefaulted reports whether these options ended up with a principal
// nobody asserts. The CLI uses it to warn, and reads the answer from the same
// resolution the manifest is rendered from rather than re-deriving it.
func (o CreateOptions) PrincipalWasDefaulted() bool {
	resolved := o.withDefaults()
	return o.PrincipalName == "" && o.KubernetesSubjectName == "" &&
		o.DefaultPrincipalToName &&
		defaultedPrincipalBindsNobody(resolved.PrincipalProvider, resolved.KubernetesSubjectKind, resolved.Name)
}

// ResolvedSystemNamespace is the namespace the manifest is created in, after
// defaulting. User-facing messages must name this, not the raw flag.
func (o CreateOptions) ResolvedSystemNamespace() string {
	return o.withDefaults().SystemNamespace
}

func (o CreateOptions) withDefaults() CreateOptions {
	if o.Namespace == "" {
		o.Namespace = o.Name
	}
	if o.SystemNamespace == "" {
		o.SystemNamespace = SystemNamespace
	}
	if o.Queue == "" {
		o.Queue = DefaultWorkspaceQueue
	}
	if o.PrincipalProvider == "" {
		o.PrincipalProvider = PrincipalProviderEntra
	}
	if o.KubernetesSubjectKind == "" {
		o.KubernetesSubjectKind = SubjectKindGroup
	}
	// Resolve the provider and subject kind first: whether the principal may
	// default at all depends on both, and reading the raw flags here would
	// decide it from a value the manifest never uses.
	//
	// The caller decides whether the flag was given at all: an explicitly-empty
	// --principal-name is a shell variable that failed to expand, not a request
	// for the default.
	if o.PrincipalName == "" && o.DefaultPrincipalToName && defaultedPrincipalBindsNobody(o.PrincipalProvider, o.KubernetesSubjectKind, o.Name) {
		o.PrincipalName = o.Name
	}
	if o.KubernetesSubjectName == "" {
		o.KubernetesSubjectName = o.PrincipalName
	}
	if o.OutputRoot == "" && o.Name != "" {
		o.OutputRoot = "/data/projects/" + o.Name + "/runs"
	}
	if o.Priority == "" {
		o.Priority = "normal"
	}
	return o
}

func validateCreateOptions(options CreateOptions) error {
	o := options.withDefaults()
	for _, name := range []struct {
		flag  string
		value string
	}{
		{flag: "name", value: o.Name},
		{flag: "--namespace", value: o.Namespace},
		{flag: "--system-namespace", value: o.SystemNamespace},
		{flag: "--queue", value: o.Queue},
	} {
		if name.value == "" {
			return fmt.Errorf("%s is required", name.flag)
		}
		if errs := validation.IsDNS1123Subdomain(name.value); len(errs) > 0 {
			return fmt.Errorf("%s %q is invalid: %s", name.flag, name.value, strings.Join(errs, "; "))
		}
	}
	for _, label := range []struct {
		flag  string
		value string
	}{
		{flag: "name", value: o.Name},
		{flag: "--queue", value: o.Queue},
	} {
		if errs := validation.IsValidLabelValue(label.value); len(errs) > 0 {
			return fmt.Errorf("%s %q cannot be used as a controller label value: %s", label.flag, label.value, strings.Join(errs, "; "))
		}
	}
	for _, namespace := range []struct {
		flag  string
		value string
	}{
		{flag: "--namespace", value: o.Namespace},
		{flag: "--system-namespace", value: o.SystemNamespace},
	} {
		if errs := validation.IsDNS1123Label(namespace.value); len(errs) > 0 {
			return fmt.Errorf("%s %q is not a valid Namespace name: %s", namespace.flag, namespace.value, strings.Join(errs, "; "))
		}
	}
	if o.Queue != DefaultWorkspaceQueue {
		return fmt.Errorf("--queue must be %q for the v0 portable workspace", DefaultWorkspaceQueue)
	}
	if o.PrincipalProvider != PrincipalProviderEntra && o.PrincipalProvider != PrincipalProviderGitHub {
		return fmt.Errorf("--principal-provider must be one of: entra, github")
	}
	if strings.TrimSpace(o.PrincipalName) == "" {
		// Name why this combination refused: the same blank principal is fine
		// on entra+Group with an ordinary name, and not otherwise.
		if entraObjectID.MatchString(o.Name) &&
			o.PrincipalProvider == PrincipalProviderEntra &&
			o.KubernetesSubjectKind == SubjectKindGroup {
			return fmt.Errorf(
				"--principal-name is required when the workspace name is object-ID-shaped; Entra asserts groups by object ID, so defaulting the subject to %q could grant a real group access",
				o.Name)
		}
		if !defaultedPrincipalBindsNobody(o.PrincipalProvider, o.KubernetesSubjectKind, o.Name) {
			return fmt.Errorf(
				"--principal-name is required for --principal-provider %s with --subject-kind %s; only an Entra Group can be named after the workspace without risking a real subject",
				o.PrincipalProvider, o.KubernetesSubjectKind)
		}
		return fmt.Errorf("--principal-name must not be blank; omit the flag entirely to default to the workspace name")
	}
	switch o.KubernetesSubjectKind {
	case SubjectKindGroup, SubjectKindUser, SubjectKindServiceAccount:
	default:
		return fmt.Errorf("--subject-kind must be one of: Group, User, ServiceAccount")
	}
	if strings.TrimSpace(o.KubernetesSubjectName) == "" {
		return fmt.Errorf("--subject-name is required")
	}
	if strings.TrimSpace(o.OutputRoot) == "" {
		return fmt.Errorf("--output-root must not be empty")
	}
	if o.Priority != "default" && o.Priority != "priority" && o.Priority != "normal" {
		return fmt.Errorf("--priority must be one of: default, priority, normal")
	}
	hasServiceAccount := strings.TrimSpace(o.ServiceAccountName) != ""
	hasClientID := strings.TrimSpace(o.WorkloadIdentityClientID) != ""
	if hasServiceAccount != hasClientID {
		return fmt.Errorf("--service-account and --workload-identity-client-id must be set together")
	}
	if hasServiceAccount {
		if errs := validation.IsDNS1123Subdomain(o.ServiceAccountName); len(errs) > 0 {
			return fmt.Errorf("--service-account %q is invalid: %s", o.ServiceAccountName, strings.Join(errs, "; "))
		}
	}
	return nil
}

func newWorkspaceForCreate(options CreateOptions) Workspace {
	o := options.withDefaults()
	workspace := Workspace{
		APIVersion: APIVersion,
		Kind:       KindWorkspace,
		Metadata: ObjectMeta{
			Name:      o.Name,
			Namespace: o.SystemNamespace,
		},
		Spec: WorkspaceSpec{
			Authorization: &WorkspaceAuthorization{Mode: AuthorizationModeWorkspaceRBAC},
			PrincipalRef: PrincipalRef{
				Provider: o.PrincipalProvider,
				Name:     o.PrincipalName,
			},
			KubernetesSubject: KubernetesSubject{
				Kind: o.KubernetesSubjectKind,
				Name: o.KubernetesSubjectName,
			},
			Role:   DefaultResearcherRole,
			Target: WorkspaceTarget{Namespace: o.Namespace, CreateNamespace: true},
			Queue:  o.Queue,
			Defaults: WorkspaceDefaults{
				OutputRoot: o.OutputRoot,
				Priority:   o.Priority,
			},
		},
	}
	if o.ServiceAccountName != "" {
		workspace.Spec.WorkloadIdentity = &WorkspaceWorkloadIdentity{
			ServiceAccountName: o.ServiceAccountName,
			ClientID:           o.WorkloadIdentityClientID,
		}
	}
	return workspace
}

func RenderCreation(options CreateOptions) ([]byte, error) {
	if err := validateCreateOptions(options); err != nil {
		return nil, err
	}
	return yaml.Marshal(newWorkspaceForCreate(options))
}

func PreflightCreation(ctx context.Context, runner AdoptRunner, options CreateOptions) (CreateReport, error) {
	if err := validateCreateOptions(options); err != nil {
		return CreateReport{}, err
	}
	o := options.withDefaults()
	raw, err := runner.Raw(ctx, []string{
		"-n", o.SystemNamespace, "get", "workspace.tau.azure.com", "-o", "json",
	}, nil)
	if err != nil {
		return CreateReport{}, fmt.Errorf("list TauWorkspaces in %q: %w", o.SystemNamespace, err)
	}
	list, err := ParseList([]byte(raw))
	if err != nil {
		return CreateReport{}, err
	}
	if len(list.Items) > 1 {
		return CreateReport{}, fmt.Errorf(
			"v0 supports one TauWorkspace, but %d already exist in %q; remove the extra objects before creating a workspace",
			len(list.Items),
			o.SystemNamespace,
		)
	}
	report := CreateReport{}
	if len(list.Items) == 1 {
		existing := list.Items[0]
		report.ExistingWorkspaceName = existing.Metadata.Name
		if existing.Metadata.DeletionTimestamp != "" {
			return CreateReport{}, fmt.Errorf(
				"TauWorkspace %q is terminating; wait for deletion to finish before creating the v0 workspace",
				existing.Metadata.Name,
			)
		}
		desired := newWorkspaceForCreate(o)
		if existing.Metadata.Name != desired.Metadata.Name || !sameAdoptionIntent(existing, desired) {
			return CreateReport{}, fmt.Errorf(
				"v0 supports one active workspace and TauWorkspace %q already exists with different intent",
				existing.Metadata.Name,
			)
		}
		report.ExistingWorkspaceIntent = "compatible"
	}

	clusterQueue, err := readCreateObject(
		ctx,
		runner,
		[]string{"get", "clusterqueue.kueue.x-k8s.io", o.Queue, "-o", "json"},
		fmt.Sprintf("ClusterQueue %q", o.Queue),
	)
	if err != nil {
		return CreateReport{}, err
	}
	if clusterQueue.Metadata.DeletionTimestamp != "" {
		return CreateReport{}, fmt.Errorf("ClusterQueue %q is terminating", o.Queue)
	}
	if clusterQueue.Metadata.UID == "" {
		return CreateReport{}, fmt.Errorf("ClusterQueue %q has no metadata.uid", o.Queue)
	}
	report.ClusterQueueUID = clusterQueue.Metadata.UID
	return report, nil
}

func ApplyCreation(
	ctx context.Context,
	runner AdoptRunner,
	options CreateOptions,
	report CreateReport,
	manifest []byte,
) (string, error) {
	if report.ExistingWorkspaceIntent == "compatible" {
		return fmt.Sprintf("TauWorkspace %q already matches the requested v0 workspace; no changes made\n", report.ExistingWorkspaceName), nil
	}
	o := options.withDefaults()
	current, err := PreflightCreation(ctx, runner, o)
	if err != nil {
		return "", fmt.Errorf("recheck workspace creation preconditions: %w", err)
	}
	if current.ExistingWorkspaceIntent == "compatible" {
		return fmt.Sprintf("TauWorkspace %q already matches the requested v0 workspace; no changes made\n", current.ExistingWorkspaceName), nil
	}
	if current.ClusterQueueUID != report.ClusterQueueUID {
		return "", fmt.Errorf(
			"ClusterQueue %q was replaced during preflight (uid %q -> %q); retry after inspecting the replacement",
			o.Queue,
			report.ClusterQueueUID,
			current.ClusterQueueUID,
		)
	}
	createArgs := []string{"-n", o.SystemNamespace, "create", "-f", "-"}
	if _, err := runner.Raw(ctx, append(append([]string(nil), createArgs...), "--dry-run=server"), manifest); err != nil {
		return "", fmt.Errorf("server-side dry-run TauWorkspace creation: %w", err)
	}
	out, err := runner.Raw(ctx, createArgs, manifest)
	if err != nil {
		return "", fmt.Errorf("create TauWorkspace conditionally: %w", err)
	}
	return out, nil
}

func readCreateObject(ctx context.Context, runner AdoptRunner, args []string, description string) (clusterQueueDocument, error) {
	raw, err := runner.Raw(ctx, args, nil)
	if err != nil {
		return clusterQueueDocument{}, fmt.Errorf("read %s: %w", description, err)
	}
	var object clusterQueueDocument
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return clusterQueueDocument{}, fmt.Errorf("parse %s: %w", description, err)
	}
	return object, nil
}
