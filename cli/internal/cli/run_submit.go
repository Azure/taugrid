package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kube"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const runSubmissionCleanupTimeout = 30 * time.Second

type runSubmission struct {
	Resource     string
	Name         string
	Namespace    string
	KubeContext  string
	SubmissionID string
	Manifest     []byte
	DryRun       string
}

type runSubmissionResult struct {
	Output    string
	Recovered bool
}

type runSubmissionCleanupRunner interface {
	kubeRawRunner
	DeleteWithUID(context.Context, runSubmission, types.UID) error
}

type runSubmissionActivationRunner interface {
	kubeRawRunner
	ActivateQueueWithUID(context.Context, runSubmission, types.UID, string) (string, error)
}

type kubernetesRunSubmissionRunner struct {
	*kube.Runner
}

func newKubernetesRunSubmissionRunner(runner *kube.Runner) *kubernetesRunSubmissionRunner {
	return &kubernetesRunSubmissionRunner{Runner: runner}
}

type runNameCollisionError struct {
	Resource    string
	Name        string
	Namespace   string
	KubeContext string
	Cause       error
}

func (e *runNameCollisionError) Error() string {
	if e.Cause != nil {
		diagnose := fmt.Sprintf("tau run diagnose %s -n %s -o json", e.Name, e.Namespace)
		if strings.TrimSpace(e.KubeContext) != "" {
			diagnose += " --context " + e.KubeContext
		}
		return fmt.Sprintf(
			"server dry-run for %s %s/%s was blocked by an existing Kubernetes object: %v. "+
				"Capture the named run evidence first with `%s`. "+
				"Choose a distinct config name, cancel the existing workload with `tau run cancel %s -n %s`, "+
				"or use `tau run resume %s --config <path>` when explicitly replacing a failed run",
			e.Resource, e.Namespace, e.Name, e.Cause,
			diagnose, e.Name, e.Namespace, e.Name,
		)
	}
	return fmt.Sprintf(
		"%s %s/%s already exists from another submission; refusing to reuse it for this tau run. "+
			"Choose a distinct config name, cancel the existing workload with `tau run cancel %s -n %s`, "+
			"or use `tau run resume %s --config <path>` when explicitly replacing a failed run",
		e.Resource, e.Namespace, e.Name, e.Name, e.Namespace, e.Name,
	)
}

func submitRunWorkload(ctx context.Context, runner kubeRawRunner, submission runSubmission) (runSubmissionResult, error) {
	if runner == nil {
		return runSubmissionResult{}, fmt.Errorf("submit run workload: runner is required")
	}
	if strings.TrimSpace(submission.Resource) == "" ||
		strings.TrimSpace(submission.Name) == "" ||
		strings.TrimSpace(submission.Namespace) == "" {
		return runSubmissionResult{}, fmt.Errorf("submit run workload: resource, name, and namespace are required")
	}
	if problems := validation.IsDNS1123Subdomain(submission.Name); len(problems) > 0 {
		return runSubmissionResult{}, fmt.Errorf("submit run workload: name %q is invalid: %s", submission.Name, strings.Join(problems, "; "))
	}
	if problems := validation.IsDNS1123Label(submission.Namespace); len(problems) > 0 {
		return runSubmissionResult{}, fmt.Errorf("submit run workload: namespace %q is invalid: %s", submission.Namespace, strings.Join(problems, "; "))
	}
	if submission.DryRun == "" && strings.TrimSpace(submission.SubmissionID) == "" {
		return runSubmissionResult{}, fmt.Errorf("submit run workload: submission ID is required")
	}

	args := []string{"create", "-n", submission.Namespace, "-f", "-"}
	if submission.DryRun != "" {
		args = append(args, "--dry-run="+submission.DryRun)
	}
	out, createErr := runner.Raw(ctx, args, submission.Manifest)
	result := runSubmissionResult{Output: out}
	if createErr == nil {
		return result, nil
	}
	if submission.DryRun != "" {
		if isKubernetesAlreadyExistsError(createErr) {
			return result, &runNameCollisionError{
				Resource:    submission.Resource,
				Name:        submission.Name,
				Namespace:   submission.Namespace,
				KubeContext: submission.KubeContext,
				Cause:       createErr,
			}
		}
		return result, createErr
	}

	existingID, lookupErr := existingRunSubmissionID(ctx, runner, submission)
	if lookupErr != nil {
		return result, fmt.Errorf(
			"create %s %s/%s: %w; could not verify whether the server accepted this submission: %v",
			submission.Resource, submission.Namespace, submission.Name, createErr, lookupErr,
		)
	}
	if existingID == submission.SubmissionID {
		result.Recovered = true
		return result, nil
	}
	return result, &runNameCollisionError{
		Resource:  submission.Resource,
		Name:      submission.Name,
		Namespace: submission.Namespace,
	}
}

func isKubernetesAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "error from server (alreadyexists):")
}

func existingRunSubmissionID(ctx context.Context, runner kubeRawRunner, submission runSubmission) (string, error) {
	metadata, err := existingRunMetadata(ctx, runner, submission)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(metadata.Annotations[workloadmeta.AnnotationSubmissionID]), nil
}

type runObjectMetadata struct {
	Annotations map[string]string `json:"annotations"`
	Labels      map[string]string `json:"labels"`
	UID         types.UID         `json:"uid"`
}

func existingRunMetadata(ctx context.Context, runner kubeRawRunner, submission runSubmission) (runObjectMetadata, error) {
	out, err := runner.Raw(ctx, []string{
		"get", submission.Resource,
		"-n", submission.Namespace,
		"-o", "json",
		"--", submission.Name,
	}, nil)
	if err != nil {
		return runObjectMetadata{}, err
	}
	var object struct {
		Metadata runObjectMetadata `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &object); err != nil {
		return runObjectMetadata{}, fmt.Errorf("decode existing %s: %w", submission.Resource, err)
	}
	return object.Metadata, nil
}

func cleanupRunSubmission(runner runSubmissionCleanupRunner, submission runSubmission) error {
	ctx, cancel := context.WithTimeout(context.Background(), runSubmissionCleanupTimeout)
	defer cancel()

	metadata, err := existingRunMetadata(ctx, runner, submission)
	if err != nil {
		if isExactObjectNotFound(err, submission.Name, submission.Resource) {
			return nil
		}
		return fmt.Errorf("verify cleanup ownership: %w", err)
	}
	existingID := strings.TrimSpace(metadata.Annotations[workloadmeta.AnnotationSubmissionID])
	if existingID != submission.SubmissionID {
		return fmt.Errorf(
			"refusing cleanup because %s %s/%s has submission ID %q, want %q",
			submission.Resource, submission.Namespace, submission.Name, existingID, submission.SubmissionID,
		)
	}
	if metadata.UID == "" {
		return fmt.Errorf("refusing cleanup because %s %s/%s has no UID", submission.Resource, submission.Namespace, submission.Name)
	}
	return runner.DeleteWithUID(ctx, submission, metadata.UID)
}

func withRunSubmissionCleanup(cause error, runner runSubmissionCleanupRunner, submissions ...runSubmission) error {
	var cleanupErrors []string
	for _, submission := range submissions {
		if err := cleanupRunSubmission(runner, submission); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s %s/%s: %v", submission.Resource, submission.Namespace, submission.Name, err))
		}
	}
	if len(cleanupErrors) == 0 {
		return cause
	}
	return fmt.Errorf("%w; cleanup failed: %s", cause, strings.Join(cleanupErrors, "; "))
}

type preparedManagedSubmission struct {
	Primary   []byte
	Ancillary []byte
	QueueName string
}

func prepareManagedSubmission(rendered []byte, primaryKind, primaryName, submissionID string) (preparedManagedSubmission, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	var prepared preparedManagedSubmission
	var ancillaryDocuments [][]byte

	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return preparedManagedSubmission{}, fmt.Errorf("decode rendered workload documents: %w", err)
		}
		if len(document) == 0 {
			continue
		}
		metadata, _ := document["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		annotations[workloadmeta.AnnotationSubmissionID] = submissionID
		kind, _ := document["kind"].(string)
		name, _ := metadata["name"].(string)
		if kind == primaryKind && name == primaryName {
			if len(prepared.Primary) > 0 {
				return preparedManagedSubmission{}, fmt.Errorf("rendered manifest contains multiple %s/%s documents", primaryKind, primaryName)
			}

			labels, _ := metadata["labels"].(map[string]any)
			if queueName, _ := labels[runtopology.QueueLabel].(string); queueName != "" {
				prepared.QueueName = queueName
				delete(labels, runtopology.QueueLabel)
				if spec, ok := document["spec"].(map[string]any); ok {
					spec["suspend"] = true
				}
			}
			encoded, err := yaml.Marshal(document)
			if err != nil {
				return preparedManagedSubmission{}, fmt.Errorf("encode gated primary workload: %w", err)
			}
			prepared.Primary = encoded
			continue
		}
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return preparedManagedSubmission{}, fmt.Errorf("encode ancillary workload document: %w", err)
		}
		ancillaryDocuments = append(ancillaryDocuments, encoded)
	}
	if len(prepared.Primary) == 0 {
		return preparedManagedSubmission{}, fmt.Errorf("rendered manifest does not contain %s/%s", primaryKind, primaryName)
	}
	prepared.Ancillary = joinYAMLDocuments(ancillaryDocuments)
	return prepared, nil
}

func (r *kubernetesRunSubmissionRunner) DeleteWithUID(ctx context.Context, submission runSubmission, uid types.UID) error {
	gvr, err := runSubmissionGVR(submission.Resource)
	if err != nil {
		return err
	}
	config, err := r.RESTConfig()
	if err != nil {
		return fmt.Errorf("resolve Kubernetes client for cleanup: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client for cleanup: %w", err)
	}
	resource := client.Resource(gvr).Namespace(submission.Namespace)
	propagation := metav1.DeletePropagationForeground
	if err := resource.Delete(ctx, submission.Name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid},
		PropagationPolicy: &propagation,
	}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		object, err := resource.Get(ctx, submission.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if object.GetUID() != uid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *kubernetesRunSubmissionRunner) ActivateQueueWithUID(ctx context.Context, submission runSubmission, uid types.UID, queueName string) (string, error) {
	patch, err := queueActivationPatch(uid, submission.SubmissionID, queueName)
	if err != nil {
		return "", err
	}
	return r.Raw(ctx, []string{
		"patch", submission.Resource + "/" + submission.Name,
		"-n", submission.Namespace,
		"--type=json",
		"-p", string(patch),
	}, nil)
}

func queueActivationPatch(uid types.UID, submissionID, queueName string) ([]byte, error) {
	operations := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(uid)},
		{"op": "test", "path": "/metadata/annotations/" + jsonPointerEscape(workloadmeta.AnnotationSubmissionID), "value": submissionID},
		{"op": "add", "path": "/metadata/labels/" + jsonPointerEscape(runtopology.QueueLabel), "value": queueName},
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		return nil, fmt.Errorf("encode queue activation patch: %w", err)
	}
	return patch, nil
}

func jsonPointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func runSubmissionGVR(resource string) (schema.GroupVersionResource, error) {
	switch strings.ToLower(strings.TrimSpace(resource)) {
	case "job", "jobs", "job.batch", "jobs.batch":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, nil
	case "rayjob", "rayjobs", "rayjob.ray.io", "rayjobs.ray.io":
		return schema.GroupVersionResource{Group: "ray.io", Version: "v1", Resource: "rayjobs"}, nil
	case "service", "services":
		return schema.GroupVersionResource{Version: "v1", Resource: "services"}, nil
	case "secret", "secrets":
		return schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unsupported cleanup resource %q", resource)
	}
}

func joinYAMLDocuments(documents [][]byte) []byte {
	var out bytes.Buffer
	for index, document := range documents {
		if index > 0 {
			out.WriteString("---\n")
		}
		out.Write(document)
		if len(document) > 0 && document[len(document)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func activateRunSubmissionQueue(ctx context.Context, runner runSubmissionActivationRunner, submission runSubmission, queueName string) (string, error) {
	if queueName == "" {
		return "", nil
	}
	metadata, err := existingRunMetadata(ctx, runner, submission)
	if err != nil {
		return "", fmt.Errorf("verify queue activation ownership: %w", err)
	}
	if metadata.Annotations[workloadmeta.AnnotationSubmissionID] != submission.SubmissionID {
		return "", fmt.Errorf(
			"refusing queue activation because %s %s/%s has submission ID %q, want %q",
			submission.Resource, submission.Namespace, submission.Name,
			metadata.Annotations[workloadmeta.AnnotationSubmissionID], submission.SubmissionID,
		)
	}
	if metadata.UID == "" {
		return "", fmt.Errorf("refusing queue activation because %s %s/%s has no UID", submission.Resource, submission.Namespace, submission.Name)
	}
	if metadata.Labels[runtopology.QueueLabel] == queueName {
		return "", nil
	}
	out, activationErr := runner.ActivateQueueWithUID(ctx, submission, metadata.UID, queueName)
	if activationErr == nil {
		return out, nil
	}
	originalUID := metadata.UID
	metadata, lookupErr := existingRunMetadata(ctx, runner, submission)
	if lookupErr == nil &&
		metadata.UID == originalUID &&
		metadata.Annotations[workloadmeta.AnnotationSubmissionID] == submission.SubmissionID &&
		metadata.Labels[runtopology.QueueLabel] == queueName {
		return out, nil
	}
	if lookupErr != nil {
		return out, fmt.Errorf("activate queue: %w; could not verify activation: %v", activationErr, lookupErr)
	}
	return out, fmt.Errorf("activate queue: %w", activationErr)
}
