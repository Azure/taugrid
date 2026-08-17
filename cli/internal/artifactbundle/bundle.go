// Package artifactbundle owns Tau's durable run-bundle completion contract.
//
// A PVC directory is storage, not a retrieval API: result publication, metrics
// offload, and checkpoint indexing finish at different times and in different
// trees. This package writes one final acknowledgement only after those nested
// producers return successfully, then describes every durable tree or glob that
// belongs to the run. Readers can therefore fail closed without mounting the PVC.
package artifactbundle

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const (
	SchemaVersion     = "tau.run.artifact-bundle.v1"
	BundleDir         = ".tau/bundles"
	CurrentManifest   = ".tau/bundle.json"
	CurrentCompletion = ".tau/bundle.complete"
)

type Publication struct {
	Mode       string `json:"mode"`
	ID         string `json:"id"`
	Root       string `json:"root"`
	Completion string `json:"completion"`
}

type Metrics struct {
	SessionID    string   `json:"session_id,omitempty"`
	History      []string `json:"history,omitempty"`
	OffloadRoot  string   `json:"offload_root,omitempty"`
	Acknowledged bool     `json:"acknowledged"`
}

type Checkpoint struct {
	Artifact string `json:"artifact,omitempty"`
	Root     string `json:"root"`
	Index    string `json:"index"`
}

type References struct {
	Artifacts  string `json:"artifacts"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Logs       string `json:"logs"`
}

type PathSpec struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Optional bool   `json:"optional,omitempty"`
}

type Manifest struct {
	SchemaVersion string       `json:"schema_version"`
	BundleID      string       `json:"bundle_id"`
	Run           string       `json:"run"`
	Namespace     string       `json:"namespace"`
	ResultPVC     string       `json:"result_pvc"`
	ResultRoot    string       `json:"result_root"`
	Publication   *Publication `json:"publication,omitempty"`
	Metrics       *Metrics     `json:"metrics,omitempty"`
	Checkpoint    *Checkpoint  `json:"checkpoint,omitempty"`
	References    References   `json:"references"`
	Paths         []PathSpec   `json:"paths"`
}

type Runtime struct {
	BundleID           string
	Run                string
	Namespace          string
	ResultPVC          string
	OutputDir          string
	PublicationMode    string
	PublicationID      string
	PublicationRoot    string
	PublicationMarker  string
	MetricsSessionID   string
	MetricsHistory     []string
	MetricsOffloadDir  string
	MetricsEnabled     bool
	CheckpointArtifact string
	CheckpointRoot     string
	CheckpointIndex    string
}

func (r Runtime) Enabled() bool {
	return strings.TrimSpace(r.BundleID) != ""
}

func (r Runtime) Validate() error {
	if !r.Enabled() {
		return nil
	}
	for name, value := range map[string]string{
		"bundle ID":  r.BundleID,
		"run":        r.Run,
		"namespace":  r.Namespace,
		"result PVC": r.ResultPVC,
		"output":     r.OutputDir,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("artifact bundle %s is required", name)
		}
	}
	id := strings.TrimSpace(r.BundleID)
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("artifact bundle ID must be a single path segment")
	}
	if err := validateDurablePath("output", r.OutputDir); err != nil {
		return err
	}
	for label, value := range map[string]string{
		"publication root":   r.PublicationRoot,
		"publication marker": r.PublicationMarker,
		"metrics offload":    r.MetricsOffloadDir,
		"checkpoint root":    r.CheckpointRoot,
		"checkpoint index":   r.CheckpointIndex,
	} {
		if strings.TrimSpace(value) != "" {
			if err := validateDurablePath(label, value); err != nil {
				return err
			}
		}
	}
	for _, history := range r.MetricsHistory {
		if err := validateDurablePath("metrics history", history); err != nil {
			return err
		}
	}
	if r.PublicationMode != "" {
		if strings.TrimSpace(r.PublicationID) == "" ||
			strings.TrimSpace(r.PublicationRoot) == "" ||
			strings.TrimSpace(r.PublicationMarker) == "" {
			return fmt.Errorf("artifact bundle publication requires ID, root, and completion marker")
		}
	}
	return nil
}

func validateDurablePath(label, value string) error {
	clean := path.Clean(strings.TrimSpace(value))
	if clean == "." || (clean != "/data" && !strings.HasPrefix(clean, "/data/")) {
		return fmt.Errorf("artifact bundle %s must be under /data", label)
	}
	return nil
}

func (r Runtime) Manifest() (Manifest, error) {
	if err := r.Validate(); err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		SchemaVersion: SchemaVersion,
		BundleID:      strings.TrimSpace(r.BundleID),
		Run:           strings.TrimSpace(r.Run),
		Namespace:     strings.TrimSpace(r.Namespace),
		ResultPVC:     strings.TrimSpace(r.ResultPVC),
		ResultRoot:    path.Clean(r.OutputDir),
		References: References{
			Artifacts: path.Clean(r.OutputDir),
			Logs:      fmt.Sprintf("tau run logs %s -n %s", strings.TrimSpace(r.Run), strings.TrimSpace(r.Namespace)),
		},
		Paths: []PathSpec{{
			Name: "results",
			Path: path.Clean(r.OutputDir),
			Kind: "tree",
		}, {
			Name: "bundle-manifest",
			Path: GenerationManifestPath(r.OutputDir, r.BundleID),
			Kind: "file",
		}, {
			Name: "bundle-acknowledgement",
			Path: GenerationCompletionPath(r.OutputDir, r.BundleID),
			Kind: "file",
		}},
	}
	if r.PublicationMode != "" {
		m.Publication = &Publication{
			Mode:       strings.TrimSpace(r.PublicationMode),
			ID:         strings.TrimSpace(r.PublicationID),
			Root:       path.Clean(r.PublicationRoot),
			Completion: path.Clean(r.PublicationMarker),
		}
		m.Paths[0].Path = path.Clean(r.PublicationRoot)
	}
	if r.MetricsEnabled {
		m.Metrics = &Metrics{
			SessionID:    strings.TrimSpace(r.MetricsSessionID),
			History:      append([]string(nil), r.MetricsHistory...),
			OffloadRoot:  cleanOptionalPath(r.MetricsOffloadDir),
			Acknowledged: true,
		}
		for i, history := range r.MetricsHistory {
			m.Paths = append(m.Paths, PathSpec{
				Name:     fmt.Sprintf("metrics-history-%d", i+1),
				Path:     path.Clean(history),
				Kind:     "glob",
				Optional: true,
			})
		}
		if strings.TrimSpace(r.MetricsOffloadDir) != "" {
			m.Paths = append(m.Paths, PathSpec{
				Name: "metrics-offload",
				Path: path.Clean(r.MetricsOffloadDir),
				Kind: "tree",
			})
		}
	}
	if strings.TrimSpace(r.CheckpointRoot) != "" {
		m.Checkpoint = &Checkpoint{
			Artifact: strings.TrimSpace(r.CheckpointArtifact),
			Root:     path.Clean(r.CheckpointRoot),
			Index:    path.Clean(r.CheckpointIndex),
		}
		m.References.Checkpoint = path.Clean(r.CheckpointIndex)
		m.Paths = append(m.Paths, PathSpec{
			Name: "checkpoint-index",
			Path: path.Clean(r.CheckpointIndex),
			Kind: "file",
		}, PathSpec{
			Name: "checkpoints",
			Path: path.Clean(r.CheckpointRoot),
			Kind: "tree",
		})
	}
	return m, nil
}

func cleanOptionalPath(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return path.Clean(value)
}

func GenerationManifestPath(outputDir, bundleID string) string {
	return path.Join(path.Clean(outputDir), BundleDir, strings.TrimSpace(bundleID)+".json")
}

func GenerationCompletionPath(outputDir, bundleID string) string {
	return path.Join(path.Clean(outputDir), BundleDir, strings.TrimSpace(bundleID)+".complete")
}

func CurrentManifestPath(outputDir string) string {
	return path.Join(path.Clean(outputDir), CurrentManifest)
}

func CurrentCompletionPath(outputDir string) string {
	return path.Join(path.Clean(outputDir), CurrentCompletion)
}

func Marshal(m Manifest) ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode artifact bundle manifest: %w", err)
	}
	if m.SchemaVersion != SchemaVersion {
		return Manifest{}, fmt.Errorf("artifact bundle schema %q is unsupported; expected %q", m.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(m.BundleID) == "" || strings.TrimSpace(m.Run) == "" ||
		strings.TrimSpace(m.Namespace) == "" ||
		strings.TrimSpace(m.ResultPVC) == "" || strings.TrimSpace(m.ResultRoot) == "" {
		return Manifest{}, fmt.Errorf("artifact bundle manifest is missing required identity fields")
	}
	if err := validateDurablePath("result root", m.ResultRoot); err != nil {
		return Manifest{}, err
	}
	if m.Metrics != nil && !m.Metrics.Acknowledged {
		return Manifest{}, fmt.Errorf("artifact bundle metrics are not acknowledged")
	}
	if m.Publication != nil {
		if strings.TrimSpace(m.Publication.ID) == "" || strings.TrimSpace(m.Publication.Completion) == "" {
			return Manifest{}, fmt.Errorf("artifact bundle publication is missing required fields")
		}
		if err := validateDurablePath("publication completion", m.Publication.Completion); err != nil {
			return Manifest{}, err
		}
	}
	if len(m.Paths) == 0 {
		return Manifest{}, fmt.Errorf("artifact bundle manifest has no durable paths")
	}
	for _, spec := range m.Paths {
		if spec.Kind != "tree" && spec.Kind != "glob" && spec.Kind != "file" {
			return Manifest{}, fmt.Errorf("artifact bundle path %q has unsupported kind %q", spec.Name, spec.Kind)
		}
		if err := validateDurablePath("path "+spec.Name, spec.Path); err != nil {
			return Manifest{}, err
		}
	}
	return m, nil
}
