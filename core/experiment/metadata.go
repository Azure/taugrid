// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	captureVersion = "v1alpha1"

	LabelRunID             = workloadmeta.LabelRunID
	LabelWorkloadKind      = workloadmeta.LabelWorkloadKind
	labelStellarProject    = workloadmeta.LabelStellarProject
	labelStellarExperiment = workloadmeta.LabelStellarExperiment
	labelStellarGroup      = workloadmeta.LabelStellarGroup

	AnnotationCaptureVersion         = workloadmeta.AnnotationCaptureVersion
	AnnotationNamespace              = workloadmeta.AnnotationNamespace
	AnnotationTauCommand             = workloadmeta.AnnotationTauCommand
	AnnotationImage                  = workloadmeta.AnnotationImage
	AnnotationImageDigest            = workloadmeta.AnnotationImageDigest
	AnnotationCodeSHA                = workloadmeta.AnnotationCodeSHA
	AnnotationConfigHash             = workloadmeta.AnnotationConfigHash
	AnnotationGPUCount               = workloadmeta.AnnotationGPUCount
	AnnotationDRAClaim               = workloadmeta.AnnotationDRAClaim
	AnnotationStorageMounts          = workloadmeta.AnnotationStorageMounts
	AnnotationStellarProject         = workloadmeta.AnnotationStellarProject
	annotationStellarExperimentTitle = workloadmeta.AnnotationStellarExperimentTitle
	AnnotationStellarExperimentID    = workloadmeta.AnnotationStellarExperimentID
	AnnotationStellarGroup           = workloadmeta.AnnotationStellarGroup
	AnnotationStellarTags            = workloadmeta.AnnotationStellarTags
	AnnotationStellarQuestion        = workloadmeta.AnnotationStellarQuestion
	AnnotationExperimentSource       = workloadmeta.AnnotationExperimentSource
	AnnotationWorkspaceID            = workloadmeta.AnnotationWorkspaceID
	AnnotationResultScope            = workloadmeta.AnnotationResultScope
	AnnotationResultPath             = workloadmeta.AnnotationResultPath
	AnnotationResultPVC              = workloadmeta.AnnotationResultPVC
	AnnotationArtifactURI            = workloadmeta.AnnotationArtifactURI
	AnnotationCheckpointURI          = workloadmeta.AnnotationCheckpointURI
	maxAnnotationValueBytes          = 8192
)

const (
	WorkloadKindJob        = "job"
	WorkloadKindRayJob     = "rayjob"
	WorkloadKindRayJobEval = "rayjob-eval"
)

type Metadata struct {
	RunID            string
	Namespace        string
	WorkspaceID      string
	ResultScope      string
	WorkloadKind     string
	TauCommand       string
	Image            string
	CodeSHA          string
	ConfigHash       string
	GPUCount         int
	DRAClaimTemplate string
	StorageMounts    []StorageMount
	Stellar          StellarMetadata
}

type StellarMetadata struct {
	Workspace    string
	Project      string
	ExperimentID string
	RunGroupID   string
	Tags         map[string]string
}

type StorageMount struct {
	Source    string `json:"source,omitempty"`
	Path      string `json:"path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
}

func (m Metadata) KubernetesMetadata() (map[string]string, map[string]string) {
	labels := map[string]string{}
	addLabel(labels, LabelRunID, m.RunID)
	addLabel(labels, LabelWorkloadKind, m.WorkloadKind)
	addLabel(labels, labelStellarProject, KubernetesLabelValue(m.Stellar.Project))
	addLabel(labels, labelStellarExperiment, KubernetesLabelValue(m.Stellar.ExperimentID))
	addLabel(labels, labelStellarGroup, KubernetesLabelValue(m.Stellar.RunGroupID))
	addLabel(labels, workloadmeta.LabelWorkspace, KubernetesLabelValue(m.Stellar.Workspace))

	annotations := map[string]string{}
	addAnnotation(annotations, AnnotationCaptureVersion, captureVersion)
	addAnnotation(annotations, AnnotationNamespace, m.Namespace)
	addAnnotation(annotations, AnnotationWorkspaceID, m.WorkspaceID)
	addAnnotation(annotations, AnnotationResultScope, m.ResultScope)
	addAnnotation(annotations, AnnotationTauCommand, m.TauCommand)
	addAnnotation(annotations, AnnotationImage, m.Image)
	addAnnotation(annotations, AnnotationImageDigest, imageDigest(m.Image))
	addAnnotation(annotations, AnnotationCodeSHA, m.CodeSHA)
	addAnnotation(annotations, AnnotationConfigHash, m.ConfigHash)
	if m.GPUCount > 0 {
		addAnnotation(annotations, AnnotationGPUCount, intString(m.GPUCount))
	}
	addAnnotation(annotations, AnnotationDRAClaim, m.DRAClaimTemplate)
	if encoded := encodeStorageMounts(m.StorageMounts); encoded != "" {
		addAnnotation(annotations, AnnotationStorageMounts, encoded)
	}
	addAnnotation(annotations, AnnotationStellarProject, m.Stellar.Project)
	addAnnotation(annotations, AnnotationStellarExperimentID, m.Stellar.ExperimentID)
	addAnnotation(annotations, AnnotationStellarGroup, m.Stellar.RunGroupID)
	stellarTags := m.Stellar.Tags
	if m.Stellar.Workspace != "" {
		stellarTags = mergeStringMaps(stellarTags, map[string]string{
			exptelemetry.TauWorkspaceTag: m.Stellar.Workspace,
		})
	}
	if encoded := encodeStellarTags(stellarTags); encoded != "" {
		addAnnotation(annotations, AnnotationStellarTags, encoded)
	}
	return labels, annotations
}

func MergeMetadata(labels, annotations map[string]string, m Metadata) (map[string]string, map[string]string) {
	captureLabels, captureAnnotations := m.KubernetesMetadata()
	outLabels := mergeStringMaps(labels, captureLabels)
	outAnnotations := mergeStringMaps(annotations, captureAnnotations)
	return outLabels, outAnnotations
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}

func HashJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}

func GitHeadSHA(dir string) string {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func imageDigest(image string) string {
	_, digest, ok := strings.Cut(strings.TrimSpace(image), "@")
	if !ok {
		return ""
	}
	if strings.HasPrefix(digest, "sha256:") && len(digest) > len("sha256:") {
		return digest
	}
	return ""
}

func encodeStorageMounts(mounts []StorageMount) string {
	if len(mounts) == 0 {
		return ""
	}
	cleaned := make([]StorageMount, 0, len(mounts))
	for _, mount := range mounts {
		mount.Source = strings.TrimSpace(mount.Source)
		mount.Path = strings.TrimSpace(mount.Path)
		mount.SourceRef = strings.TrimSpace(mount.SourceRef)
		if mount.Path == "" {
			continue
		}
		cleaned = append(cleaned, mount)
	}
	if len(cleaned) == 0 {
		return ""
	}
	sort.Slice(cleaned, func(i, j int) bool {
		if cleaned[i].Path == cleaned[j].Path {
			return cleaned[i].SourceRef < cleaned[j].SourceRef
		}
		return cleaned[i].Path < cleaned[j].Path
	})
	data, err := json.Marshal(cleaned)
	if err != nil || len(data) > maxAnnotationValueBytes {
		return ""
	}
	return string(data)
}

func encodeStellarTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	cleaned := map[string]string{}
	for key, value := range tags {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		cleaned[key] = strings.TrimSpace(value)
	}
	if len(cleaned) == 0 {
		return ""
	}
	data, err := json.Marshal(cleaned)
	if err != nil {
		return ""
	}
	if len(data) > maxAnnotationValueBytes {
		workspace := cleaned[exptelemetry.TauWorkspaceTag]
		if workspace == "" {
			return ""
		}
		data, err = json.Marshal(map[string]string{
			exptelemetry.TauWorkspaceTag: workspace,
		})
		if err != nil || len(data) > maxAnnotationValueBytes {
			return ""
		}
	}
	return string(data)
}

func RedactCommandArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	out := make([]string, 0, len(args))
	redactNext := false
	for _, arg := range args {
		if redactNext {
			out = append(out, redactSensitiveValue(out[len(out)-1], arg))
			redactNext = false
			continue
		}
		if strings.HasPrefix(arg, "--") {
			flag, value, hasValue := strings.Cut(arg, "=")
			name := strings.TrimLeft(flag, "-")
			if isSensitiveFlag(name) {
				if hasValue {
					out = append(out, flag+"="+redactValueForFlag(name, value))
				} else {
					out = append(out, flag)
					redactNext = true
				}
				continue
			}
		}
		out = append(out, arg)
	}
	return shellJoin(out)
}

func redactEnvAssignment(value string) string {
	key, _, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return "<redacted>"
	}
	return key + "=<redacted>"
}

func KubernetesLabelValue(value string) string {
	original := strings.TrimSpace(value)
	if original == "" {
		return ""
	}
	lowered := strings.ToLower(original)
	var b strings.Builder
	lastDash := false
	for _, r := range lowered {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	normalized := strings.Trim(b.String(), "-_.")
	if normalized == "" {
		normalized = shortLabelHash(original)
	}
	if len(normalized) <= 63 && validLabelValue(normalized) {
		return normalized
	}
	suffix := "-" + shortLabelHash(original)
	limit := 63 - len(suffix)
	if limit < 1 {
		return strings.Trim(suffix, "-")
	}
	normalized = strings.Trim(normalized[:min(limit, len(normalized))], "-_.")
	if normalized == "" {
		return strings.Trim(suffix, "-")
	}
	return normalized + suffix
}

func shortLabelHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func addLabel(out map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if key == "" || value == "" || len(value) > 63 || !validLabelValue(value) {
		return
	}
	out[key] = value
}

func addAnnotation(out map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if key == "" || value == "" || len(value) > maxAnnotationValueBytes {
		return
	}
	out[key] = value
}

func mergeStringMaps(first, second map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range first {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	for k, v := range second {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func validLabelValue(v string) bool {
	for i, r := range v {
		if unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		if unicode.IsUpper(r) {
			return false
		}
		if i == 0 || i == len(v)-1 {
			return false
		}
		return false
	}
	first := v[0]
	last := v[len(v)-1]
	return isLabelEdge(first) && isLabelEdge(last)
}

func isLabelEdge(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func isSensitiveFlag(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if name == "env" {
		return true
	}
	for _, token := range []string{"secret", "token", "password", "passwd", "pat", "credential", "api-key", "apikey"} {
		if strings.Contains(name, token) {
			return true
		}
	}
	return false
}

func redactSensitiveValue(flag, value string) string {
	name := strings.TrimLeft(flag, "-")
	return redactValueForFlag(name, value)
}

func redactValueForFlag(name, value string) string {
	if strings.EqualFold(name, "env") {
		return redactEnvAssignment(value)
	}
	return "<redacted>"
}

func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = shellQuote(arg)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_./:=,@+", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
