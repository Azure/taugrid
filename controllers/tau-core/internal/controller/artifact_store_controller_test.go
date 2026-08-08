package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestArtifactStoreReconcilerStampsBlobIdentity(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "blob-training", Namespace: "research"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-123"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-123"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           azureBlobCSIDriver,
					VolumeHandle:     "rg#trainingacct#results#uuid#research#subscription",
					VolumeAttributes: map[string]string{"storageEndpointSuffix": "core.windows.net"},
				},
			},
		},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "training-1",
		Namespace: "research",
		Labels:    map[string]string{labelkeys.LabelManagedBy: "tau"},
		Annotations: map[string]string{
			labelkeys.AnnotationArtifactBundleID: "bundle-1",
			labelkeys.AnnotationResultPVC:        "blob-training",
			labelkeys.AnnotationArtifactStore:    `{"schema_version":"tau.run.blob-volume.v1","account_url":"https://attacker.example","container":"stolen"}`,
		},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, job).Build()
	reconciler := &artifactStoreReconciler{
		Client:    c,
		newObject: func() client.Object { return &batchv1.Job{} },
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "research", Name: "training-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var got batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "research", Name: "training-1"}, &got); err != nil {
		t.Fatal(err)
	}
	raw := got.Annotations[labelkeys.AnnotationArtifactStore]
	var document artifactStoreDocument
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		t.Fatalf("decode stamped annotation %q: %v", raw, err)
	}
	if document.SchemaVersion != artifactStoreSchema ||
		document.AccountURL != "https://trainingacct.blob.core.windows.net" ||
		document.Container != "results" {
		t.Fatalf("artifact store document = %+v", document)
	}
}

func TestArtifactStoreFromPVRejectsUntrustedEndpoint(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "hostile"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: azureBlobCSIDriver,
					VolumeAttributes: map[string]string{
						"storageAccount": "trainingacct",
						"containerName":  "results",
						"server":         "attacker.example",
					},
				},
			},
		},
	}
	_, supported, err := artifactStoreFromPV(pv)
	if err != nil {
		t.Fatal(err)
	}
	if supported {
		t.Fatal("untrusted endpoint was marked supported")
	}
}
