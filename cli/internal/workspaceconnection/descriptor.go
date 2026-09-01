// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package workspaceconnection owns Tau's non-secret repository-to-workspace
// connection contract and local verified connection state.
package workspaceconnection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/taugrid/cli/internal/repository"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

const (
	DescriptorSchema                            = "tau.workspace.connection.v1"
	DescriptorRelativePath                      = "tau/workspace.connection.yaml"
	AccessMethodKubeconfig         AccessMethod = "kubeconfig"
	AccessMethodAKS                AccessMethod = "aks"
	AuthorizationModeClusterWide                = "cluster-wide"
	AuthorizationModeWorkspaceRBAC              = "workspace-rbac"
)

var (
	ErrDescriptorNotFound = errors.New("Tau workspace connection descriptor not found")
	uuidPattern           = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type ClusterDescriptor struct {
	ContextName     string `yaml:"contextName" json:"contextName"`
	SystemNamespace string `yaml:"systemNamespace,omitempty" json:"systemNamespace,omitempty"`
}

type AKSAccessDescriptor struct {
	ResourceID string `yaml:"resourceID" json:"resourceID"`
	TenantID   string `yaml:"tenantID" json:"tenantID"`
}

type AccessMethod string

type AccessDescriptor struct {
	Method AccessMethod         `yaml:"method" json:"method"`
	AKS    *AKSAccessDescriptor `yaml:"aks,omitempty" json:"aks,omitempty"`
}

type AuthorizationDescriptor struct {
	Mode         string `yaml:"mode" json:"mode"`
	RequiredRole string `yaml:"requiredRole,omitempty" json:"requiredRole,omitempty"`
}

type RequirementsDescriptor struct {
	MinTauVersion string `yaml:"minTauVersion" json:"minTauVersion"`
}

type NetworkDescriptor struct {
	PrivateCluster bool   `yaml:"privateCluster" json:"privateCluster"`
	Instructions   string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
}

type Descriptor struct {
	Schema        string                  `yaml:"schema" json:"schema"`
	Workspace     string                  `yaml:"workspace" json:"workspace"`
	Cluster       ClusterDescriptor       `yaml:"cluster" json:"cluster"`
	Access        AccessDescriptor        `yaml:"access" json:"access"`
	Authorization AuthorizationDescriptor `yaml:"authorization" json:"authorization"`
	Requirements  RequirementsDescriptor  `yaml:"requirements" json:"requirements"`
	Network       NetworkDescriptor       `yaml:"network" json:"network"`
}

type Discovery struct {
	Path           string
	RepositoryRoot string
	Descriptor     Descriptor
	Digest         string
}

func Parse(raw []byte) (Descriptor, error) {
	var descriptor Descriptor
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("parse %s: %w", DescriptorRelativePath, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Descriptor{}, fmt.Errorf("parse %s: multiple YAML documents are not allowed", DescriptorRelativePath)
		}
		return Descriptor{}, fmt.Errorf("parse %s: %w", DescriptorRelativePath, err)
	}
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

func (d Descriptor) Validate() error {
	if d.Schema != DescriptorSchema {
		return fmt.Errorf("workspace connection schema %q is unsupported; expected %q", d.Schema, DescriptorSchema)
	}
	if strings.TrimSpace(d.Workspace) == "" {
		return fmt.Errorf("workspace connection workspace is required")
	}
	if problems := validation.IsDNS1123Subdomain(d.Workspace); len(problems) > 0 {
		return fmt.Errorf("workspace connection workspace %q is invalid: %s", d.Workspace, strings.Join(problems, "; "))
	}
	if strings.TrimSpace(d.Cluster.ContextName) == "" {
		return fmt.Errorf("workspace connection cluster.contextName is required")
	}
	if namespace := strings.TrimSpace(d.Cluster.SystemNamespace); namespace != "" {
		if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
			return fmt.Errorf("workspace connection cluster.systemNamespace %q is invalid: %s", namespace, strings.Join(problems, "; "))
		}
	}
	accessDefinition, ok := accessMethodDefinitionFor(d.Access.Method)
	if !ok {
		return fmt.Errorf(
			"workspace connection access.method must be one of: %s",
			strings.Join(supportedAccessMethodNames(), ", "),
		)
	}
	for _, definition := range accessMethodDefinitions() {
		if definition.method == d.Access.Method ||
			definition.hasMetadata == nil ||
			!definition.hasMetadata(d.Access) {
			continue
		}
		return fmt.Errorf(
			"workspace connection access.%s must be omitted for method %s",
			definition.metadataField,
			d.Access.Method,
		)
	}
	if err := accessDefinition.validate(d.Access); err != nil {
		return err
	}
	switch d.Authorization.Mode {
	case AuthorizationModeClusterWide:
		if strings.TrimSpace(d.Authorization.RequiredRole) != "" {
			return fmt.Errorf(
				"workspace connection authorization.requiredRole must be omitted for mode %s because the workspace is not an authorization boundary",
				AuthorizationModeClusterWide,
			)
		}
	case AuthorizationModeWorkspaceRBAC:
		if strings.TrimSpace(d.Authorization.RequiredRole) == "" {
			return fmt.Errorf("workspace connection authorization.requiredRole is required for mode %s", AuthorizationModeWorkspaceRBAC)
		}
	default:
		return fmt.Errorf(
			"workspace connection authorization.mode must be one of: %s, %s",
			AuthorizationModeClusterWide,
			AuthorizationModeWorkspaceRBAC,
		)
	}
	if strings.TrimSpace(d.Requirements.MinTauVersion) == "" {
		return fmt.Errorf("workspace connection requirements.minTauVersion is required")
	}
	if _, err := parseVersion(d.Requirements.MinTauVersion); err != nil {
		return fmt.Errorf("workspace connection requirements.minTauVersion: %w", err)
	}
	if d.Network.PrivateCluster && strings.TrimSpace(d.Network.Instructions) == "" {
		return fmt.Errorf("workspace connection network.instructions is required for a private cluster")
	}
	return nil
}

func (d Descriptor) ResolvedSystemNamespace() string {
	if namespace := strings.TrimSpace(d.Cluster.SystemNamespace); namespace != "" {
		return namespace
	}
	return tauworkspace.SystemNamespace
}

func (d Descriptor) AccessIdentity() string {
	definition, ok := accessMethodDefinitionFor(d.Access.Method)
	if !ok {
		return ""
	}
	return definition.identity(d)
}

type accessMethodDefinition struct {
	method                 AccessMethod
	metadataField          string
	hasMetadata            func(AccessDescriptor) bool
	validate               func(AccessDescriptor) error
	identity               func(Descriptor) string
	tracksKubeconfigSource bool
}

func accessMethodDefinitions() []accessMethodDefinition {
	return []accessMethodDefinition{
		{
			method:   AccessMethodKubeconfig,
			validate: func(AccessDescriptor) error { return nil },
			identity: func(descriptor Descriptor) string {
				return string(AccessMethodKubeconfig) + ":" + strings.TrimSpace(descriptor.Cluster.ContextName)
			},
			tracksKubeconfigSource: true,
		},
		{
			method:        AccessMethodAKS,
			metadataField: "aks",
			hasMetadata: func(access AccessDescriptor) bool {
				return access.AKS != nil
			},
			validate: validateAKSAccess,
			identity: func(descriptor Descriptor) string {
				if descriptor.Access.AKS == nil {
					return ""
				}
				return string(AccessMethodAKS) + ":" +
					strings.ToLower(strings.TrimSpace(descriptor.Access.AKS.ResourceID)) + ":" +
					strings.ToLower(strings.TrimSpace(descriptor.Access.AKS.TenantID))
			},
		},
	}
}

func accessMethodDefinitionFor(method AccessMethod) (accessMethodDefinition, bool) {
	for _, definition := range accessMethodDefinitions() {
		if definition.method == method {
			return definition, true
		}
	}
	return accessMethodDefinition{}, false
}

func supportedAccessMethodNames() []string {
	definitions := accessMethodDefinitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, string(definition.method))
	}
	return names
}

func validateAKSAccess(access AccessDescriptor) error {
	if access.AKS == nil {
		return fmt.Errorf("workspace connection access.aks is required for method %s", AccessMethodAKS)
	}
	id, err := arm.ParseResourceID(strings.TrimSpace(access.AKS.ResourceID))
	if err != nil {
		return fmt.Errorf("workspace connection access.aks.resourceID: %w", err)
	}
	if !strings.EqualFold(id.ResourceType.Namespace, "Microsoft.ContainerService") ||
		!strings.EqualFold(id.ResourceType.Type, "managedClusters") ||
		id.SubscriptionID == "" || id.ResourceGroupName == "" || id.Name == "" {
		return fmt.Errorf("workspace connection access.aks.resourceID must identify an AKS managed cluster")
	}
	if !uuidPattern.MatchString(id.SubscriptionID) {
		return fmt.Errorf("workspace connection access.aks.resourceID has invalid subscription ID %q", id.SubscriptionID)
	}
	if !uuidPattern.MatchString(strings.TrimSpace(access.AKS.TenantID)) {
		return fmt.Errorf("workspace connection access.aks.tenantID must be a UUID")
	}
	return nil
}

func (d Descriptor) tracksKubeconfigSource() bool {
	definition, ok := accessMethodDefinitionFor(d.Access.Method)
	return ok && definition.tracksKubeconfigSource
}

func CheckTauVersion(current, minimum string) error {
	if strings.EqualFold(strings.TrimSpace(current), "dev") {
		return nil
	}
	currentVersion, err := parseVersion(current)
	if err != nil {
		return fmt.Errorf("parse installed Tau version: %w", err)
	}
	minimumVersion, err := parseVersion(minimum)
	if err != nil {
		return fmt.Errorf("parse required Tau version: %w", err)
	}
	for i := range currentVersion {
		if currentVersion[i] > minimumVersion[i] {
			return nil
		}
		if currentVersion[i] < minimumVersion[i] {
			return fmt.Errorf("Tau %s is too old for this repository; install Tau %s or newer", current, minimum)
		}
	}
	return nil
}

func parseVersion(value string) ([3]int, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if base, _, ok := strings.Cut(value, "-"); ok {
		value = base
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("%q must use major.minor.patch", value)
	}
	var parsed [3]int
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, fmt.Errorf("%q must use non-negative numeric major.minor.patch", value)
		}
		parsed[i] = number
	}
	return parsed, nil
}

func Digest(descriptor Descriptor) (string, error) {
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return "", fmt.Errorf("marshal workspace connection descriptor: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// LoadFile parses one exact descriptor path without ancestor discovery.
func LoadFile(path, repositoryRoot string) (Discovery, error) {
	absolute, realPath, info, err := repository.ExistingPath(path)
	if err != nil {
		return Discovery{}, fmt.Errorf("read workspace connection descriptor %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Discovery{}, fmt.Errorf("workspace connection descriptor %s is not a regular file", absolute)
	}
	if strings.TrimSpace(repositoryRoot) == "" {
		repositoryRoot = filepath.Dir(absolute)
	}
	absoluteRoot, realRoot, rootInfo, err := repository.ExistingPath(repositoryRoot)
	if err != nil {
		return Discovery{}, fmt.Errorf("resolve workspace connection repository root %s: %w", repositoryRoot, err)
	}
	if !rootInfo.IsDir() {
		return Discovery{}, fmt.Errorf("workspace connection repository root %s is not a directory", realRoot)
	}
	physicalContained, err := repository.PathContains(realRoot, realPath)
	if err != nil {
		return Discovery{}, fmt.Errorf("compare workspace connection descriptor to repository root: %w", err)
	}
	if !repository.Contains(absoluteRoot, absolute) || !physicalContained {
		return Discovery{}, fmt.Errorf("workspace connection descriptor %s escapes repository root %s", absolute, absoluteRoot)
	}
	raw, err := os.ReadFile(absolute)
	if err != nil {
		return Discovery{}, fmt.Errorf("read workspace connection descriptor %s: %w", absolute, err)
	}
	descriptor, err := Parse(raw)
	if err != nil {
		return Discovery{}, err
	}
	digest, err := Digest(descriptor)
	if err != nil {
		return Discovery{}, err
	}
	return Discovery{
		Path:           absolute,
		RepositoryRoot: absoluteRoot,
		Descriptor:     descriptor,
		Digest:         digest,
	}, nil
}

func Discover(start string) (Discovery, error) {
	boundary, err := repository.Resolve(start)
	if err != nil {
		return Discovery{}, err
	}
	lexicalCurrent := boundary.LexicalStartDir
	for {
		path := filepath.Join(lexicalCurrent, filepath.FromSlash(DescriptorRelativePath))
		_, readErr := os.Stat(path)
		switch {
		case readErr == nil:
			return LoadFile(path, boundary.LexicalRoot)
		case !os.IsNotExist(readErr):
			return Discovery{}, fmt.Errorf("inspect workspace connection descriptor %s: %w", path, readErr)
		}
		if lexicalCurrent == boundary.LexicalRoot {
			break
		}
		lexicalParent := filepath.Dir(lexicalCurrent)
		if lexicalParent == lexicalCurrent || !repository.Contains(boundary.LexicalRoot, lexicalParent) {
			break
		}
		lexicalCurrent = lexicalParent
	}
	return Discovery{}, fmt.Errorf(
		"%w: expected %s between %s and repository boundary %s",
		ErrDescriptorNotFound,
		DescriptorRelativePath,
		boundary.LexicalStartDir,
		boundary.LexicalRoot,
	)
}
