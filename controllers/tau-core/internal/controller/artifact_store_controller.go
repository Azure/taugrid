package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	artifactStoreSchema = "tau.run.blob-volume.v1"
	azureBlobCSIDriver  = "blob.csi.azure.com"
)

type artifactStoreReconciler struct {
	client.Client
	newObject func() client.Object
}

type artifactStoreDocument struct {
	SchemaVersion string `json:"schema_version"`
	AccountURL    string `json:"account_url"`
	Container     string `json:"container"`
}

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=ray.io,resources=rayjobs,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *artifactStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workload := r.newObject()
	if err := r.Get(ctx, req.NamespacedName, workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !artifactStoreCandidate(workload) {
		return ctrl.Result{}, nil
	}

	pvcName := strings.TrimSpace(workload.GetAnnotations()[labelkeys.AnnotationResultPVC])
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: pvcName}, &pvc); err != nil {
		return ctrl.Result{}, err
	}
	if strings.TrimSpace(pvc.Spec.VolumeName) == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return ctrl.Result{}, err
	}
	document, supported, err := artifactStoreFromPV(&pv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !supported {
		return ctrl.Result{}, nil
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("encode artifact store metadata: %w", err)
	}
	if workload.GetAnnotations()[labelkeys.AnnotationArtifactStore] == string(raw) {
		return ctrl.Result{}, nil
	}
	before := workload.DeepCopyObject().(client.Object)
	annotations := workload.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[labelkeys.AnnotationArtifactStore] = string(raw)
	workload.SetAnnotations(annotations)
	if err := r.Patch(ctx, workload, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("stamp artifact store metadata on %s/%s: %w", req.Namespace, req.Name, err)
	}
	return ctrl.Result{}, nil
}

func artifactStoreCandidate(object client.Object) bool {
	return object.GetLabels()[labelkeys.LabelManagedBy] == "tau" &&
		strings.TrimSpace(object.GetAnnotations()[labelkeys.AnnotationArtifactBundleID]) != "" &&
		strings.TrimSpace(object.GetAnnotations()[labelkeys.AnnotationResultPVC]) != ""
}

func artifactStoreFromPV(pv *corev1.PersistentVolume) (artifactStoreDocument, bool, error) {
	if pv.Spec.CSI == nil || !strings.EqualFold(strings.TrimSpace(pv.Spec.CSI.Driver), azureBlobCSIDriver) {
		return artifactStoreDocument{}, false, nil
	}
	attributes, err := foldArtifactStoreAttributes(pv.Spec.CSI.VolumeAttributes)
	if err != nil {
		return artifactStoreDocument{}, false, fmt.Errorf("PV %s: %w", pv.Name, err)
	}
	account := strings.TrimSpace(attributes["storageaccount"])
	containerName := strings.TrimSpace(attributes["containername"])
	parts := strings.Split(pv.Spec.CSI.VolumeHandle, "#")
	if account == "" && len(parts) > 1 {
		account = strings.TrimSpace(parts[1])
	}
	if containerName == "" && len(parts) > 2 {
		containerName = strings.TrimSpace(parts[2])
	}
	if account == "" || containerName == "" || strings.ContainsAny(containerName, `/\`) {
		return artifactStoreDocument{}, false, nil
	}
	server := strings.TrimSpace(attributes["server"])
	if server == "" {
		server = account + ".blob." + firstArtifactStoreValue(attributes["storageendpointsuffix"], "core.windows.net")
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return artifactStoreDocument{}, false, fmt.Errorf("PV %s has invalid Blob server %q", pv.Name, server)
	}
	accountURL := strings.TrimSuffix(parsed.String(), "/")
	if !trustedArtifactStoreHost(accountURL) {
		return artifactStoreDocument{}, false, nil
	}
	return artifactStoreDocument{
		SchemaVersion: artifactStoreSchema,
		AccountURL:    accountURL,
		Container:     containerName,
	}, true, nil
}

func foldArtifactStoreAttributes(attributes map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(attributes))
	for key, value := range attributes {
		folded := strings.ToLower(strings.TrimSpace(key))
		if previous, exists := out[folded]; exists && previous != value {
			return nil, fmt.Errorf("volumeAttributes contains conflicting case variants for %q", key)
		}
		out[folded] = value
	}
	return out, nil
}

func trustedArtifactStoreHost(accountURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(accountURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	for _, suffix := range []string{
		".blob.core.windows.net",
		".blob.core.usgovcloudapi.net",
		".blob.core.chinacloudapi.cn",
		".blob.core.cloudapi.de",
	} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func firstArtifactStoreValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func artifactStorePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return artifactStoreCandidate(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return artifactStoreCandidate(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return artifactStoreCandidate(e.Object) },
	}
}

func SetupArtifactStoreControllers(mgr ctrl.Manager) error {
	job := &artifactStoreReconciler{
		Client:    mgr.GetClient(),
		newObject: func() client.Object { return &batchv1.Job{} },
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("job-artifact-store").
		For(&batchv1.Job{}, builder.WithPredicates(artifactStorePredicate())).
		Complete(job); err != nil {
		return err
	}

	rayJob := &unstructured.Unstructured{}
	rayJob.SetGroupVersionKind(schema.GroupVersionKind{Group: "ray.io", Version: "v1", Kind: "RayJob"})
	if _, err := mgr.GetRESTMapper().RESTMapping(rayJob.GroupVersionKind().GroupKind(), rayJob.GroupVersionKind().Version); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("discover RayJob API for artifact store controller: %w", err)
	}
	ray := &artifactStoreReconciler{
		Client: mgr.GetClient(),
		newObject: func() client.Object {
			object := &unstructured.Unstructured{}
			object.SetGroupVersionKind(rayJob.GroupVersionKind())
			return object
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("rayjob-artifact-store").
		For(rayJob, builder.WithPredicates(artifactStorePredicate())).
		Complete(ray)
}
