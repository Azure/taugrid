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
	DefaultResearcherRole = "researcher"
	LegacyResearcherRole  = "tau-researcher-v1"

	PrincipalProviderEntra  = "entra"
	PrincipalProviderGitHub = "github"

	SubjectKindGroup          = "Group"
	SubjectKindUser           = "User"
	SubjectKindServiceAccount = "ServiceAccount"

	DefaultWorkspaceName = workloadmeta.DefaultWorkspaceName
)

func IsResearcherRole(role string) bool {
	return role == DefaultResearcherRole || role == LegacyResearcherRole
}

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

var entraObjectID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func defaultedPrincipalBindsNobody(provider, subjectKind, name string) bool {
	return provider == PrincipalProviderEntra &&
		subjectKind == SubjectKindGroup &&
		!entraObjectID.MatchString(name)
}

func (o CreateOptions) PrincipalWasDefaulted() bool {
	resolved := o.withDefaults()
	return o.PrincipalName == "" && o.KubernetesSubjectName == "" &&
		o.DefaultPrincipalToName &&
		defaultedPrincipalBindsNobody(resolved.PrincipalProvider, resolved.KubernetesSubjectKind, resolved.Name)
}

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
