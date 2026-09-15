// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRenderBoundaryRequiresEveryExplicitApprovalInput(t *testing.T) {
	source, err := e2e.ReadRepoFile("tests/e2e/stack/fixtures/nccl-rdma-security-boundary.yaml")
	require.NoError(t, err)
	config := testCheckConfig()
	rendered, err := renderBoundary(source, "print('probe')\n", config)
	require.NoError(t, err)
	require.NotRegexp(t, approvalPlaceholderRE, string(rendered))

	config.jobController = "APPROVED_JOB_CONTROLLER_USERNAME"
	require.ErrorContains(t, config.validate(), "unresolved approval placeholder")

	config = testCheckConfig()
	config.untrusted = config.operator
	require.ErrorContains(t, config.validate(), "must be distinct")
}

func TestValidateActiveBoundaryFailsClosedForMissingDriftAndBypassedBinding(t *testing.T) {
	probe, err := e2e.ReadRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	require.NoError(t, err)
	expected := testBoundaryDocumentsWithProbe(t, string(probe))

	t.Run("exact active boundary", func(t *testing.T) {
		require.NoError(t, validateActiveBoundary(
			context.Background(), fakeBoundaryClient(t, expected, nil), expected,
		))
	})
	t.Run("missing policy", func(t *testing.T) {
		require.ErrorContains(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				require.NoError(t, client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Delete(context.Background(), "taugrid-nccl-rdma-job-boundary", metav1.DeleteOptions{}))
			}),
			expected,
		), "is not active")
	})
	t.Run("drifted policy", func(t *testing.T) {
		require.ErrorContains(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Get(context.Background(), "taugrid-nccl-rdma-job-boundary", metav1.GetOptions{})
				require.NoError(t, err)
				policy.Spec.Validations[0].Expression = "true"
				_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Update(context.Background(), policy, metav1.UpdateOptions{})
				require.NoError(t, err)
			}),
			expected,
		), "drifts")
	})
	t.Run("non-Deny binding", func(t *testing.T) {
		require.ErrorContains(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
					Get(context.Background(), "taugrid-nccl-rdma-connect-deny", metav1.GetOptions{})
				require.NoError(t, err)
				binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
				_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
					Update(context.Background(), binding, metav1.UpdateOptions{})
				require.NoError(t, err)
			}),
			expected,
		), "drifts")
	})
	t.Run("type-check warning", func(t *testing.T) {
		require.ErrorContains(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Get(context.Background(), "taugrid-nccl-rdma-secret-boundary", metav1.GetOptions{})
				require.NoError(t, err)
				policy.Status.TypeChecking.ExpressionWarnings = append(
					policy.Status.TypeChecking.ExpressionWarnings,
					admissionv1.ExpressionWarning{FieldRef: "spec.validations[0]", Warning: "bad expression"},
				)
				_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					UpdateStatus(context.Background(), policy, metav1.UpdateOptions{})
				require.NoError(t, err)
			}),
			expected,
		), "type-check warnings")
	})
	t.Run("Kubernetes 1.34 equivalent defaults", func(t *testing.T) {
		require.NoError(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				for name := range expected.policies {
					policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
						Get(context.Background(), name, metav1.GetOptions{})
					require.NoError(t, err)
					policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{}
					policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}
					for index := range policy.Spec.MatchConstraints.ResourceRules {
						scope := admissionv1.AllScopes
						policy.Spec.MatchConstraints.ResourceRules[index].Scope = &scope
					}
					_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
						Update(context.Background(), policy, metav1.UpdateOptions{})
					require.NoError(t, err)
				}
			}),
			expected,
		))
	})
	t.Run("non-empty selector drift is not normalized", func(t *testing.T) {
		require.ErrorContains(t, validateActiveBoundary(
			context.Background(),
			fakeBoundaryClient(t, expected, func(client kubernetes.Interface) {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Get(context.Background(), "taugrid-nccl-rdma-job-boundary", metav1.GetOptions{})
				require.NoError(t, err)
				policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"bypass": "true"},
				}
				_, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Update(context.Background(), policy, metav1.UpdateOptions{})
				require.NoError(t, err)
			}),
			expected,
		), "drifts")
	})
}

func TestGeneratedPodProbeRetainsManualSelectorLabels(t *testing.T) {
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "e2e-nccl-rdma-2x1xh200",
			Namespace: boundaryNamespace,
		},
		Spec: batchv1.JobSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"batch.kubernetes.io/job-name":     "e2e-nccl-rdma-2x1xh200",
				"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
				e2e.NCCLRDMAInvocationKey:          "nccl-rdma-0123456789abcdef0123456789abcdef",
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":           "nccl-rdma-diagnostic",
					"batch.kubernetes.io/job-name":     "e2e-nccl-rdma-2x1xh200",
					"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
					e2e.NCCLRDMAInvocationKey:          "nccl-rdma-0123456789abcdef0123456789abcdef",
				}},
			},
		},
	}
	pod := generatedPodProbe(&job, 1)
	for key, value := range job.Spec.Selector.MatchLabels {
		require.Equal(t, value, pod.Labels[key])
	}
	require.Equal(t, "1", pod.Labels["batch.kubernetes.io/job-completion-index"])
	require.Equal(t, "e2e-nccl-rdma-2x1xh200-1-", pod.GenerateName)
	require.Equal(t, "e2e-nccl-rdma-2x1xh200-1", pod.Spec.Hostname)
}

func TestNCCLRDMABoundaryCompilesOnLocalAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to installed envtest binaries for API-server CEL compilation")
	}
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, environment.Stop())
	})
	client, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: boundaryNamespace,
			Labels: map[string]string{
				"tau.azure.com/nccl-rdma-security-boundary": "v3",
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	probe, err := e2e.ReadRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	require.NoError(t, err)
	expected := testBoundaryDocumentsWithProbe(t, string(probe))
	for _, policy := range expected.policies {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
			Create(ctx, policy.DeepCopy(), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	for _, binding := range expected.bindings {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
			Create(ctx, binding.DeepCopy(), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	generatedPolicy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
		Get(ctx, "taugrid-nccl-rdma-generated-pods-boundary", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, generatedPolicy.Spec.MatchConstraints.NamespaceSelector)
	require.NotNil(t, generatedPolicy.Spec.MatchConstraints.ObjectSelector)
	require.Equal(t, admissionv1.AllScopes, *generatedPolicy.Spec.MatchConstraints.ResourceRules[0].Scope)
	require.Equal(
		t,
		canonicalPolicySpec(expected.policies[generatedPolicy.Name].Spec),
		canonicalPolicySpec(generatedPolicy.Spec),
	)

	config := testCheckConfig()
	for index, username := range []string{
		config.operator, config.kueueController, config.jobController, config.untrusted,
	} {
		_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nccl-rdma-envtest-%d", index)},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "cluster-admin",
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     username,
			}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	jobSource, err := e2e.ReadRepoFile("tests/e2e/stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml")
	require.NoError(t, err)
	job, err := renderJob(jobSource, config)
	require.NoError(t, err)
	operatorDynamic, err := dynamic.NewForConfig(impersonated(restConfig, config.operator))
	require.NoError(t, err)
	createdJob, err := jobsFor(operatorDynamic, config.namespace).Create(ctx, job, metav1.CreateOptions{})
	require.NoError(t, err, "exact suspended Job must pass local API admission")
	support, err := e2e.BuildNCCLRDMASupportResources(
		config.namespace, config.invocation, string(probe), make([]byte, 32),
	)
	require.NoError(t, err)
	require.NoError(t, dryRunSupportCreates(ctx, operatorDynamic, config.namespace, support))

	untrustedDynamic, err := dynamic.NewForConfig(impersonated(restConfig, config.untrusted))
	require.NoError(t, err)
	maliciousJob := job.DeepCopy()
	maliciousJob.SetName("e2e-nccl-rdma-attacker")
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx, 100*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err = jobsFor(untrustedDynamic, config.namespace).Create(
				ctx, maliciousJob, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
			)
			if err == nil {
				return false, nil
			}
			return true, expectDenied(err, "taugrid-nccl-rdma-job-boundary")
		},
	))

	kueueDynamic, err := dynamic.NewForConfig(impersonated(restConfig, config.kueueController))
	require.NoError(t, err)
	kueueUpdate := createdJob.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(kueueUpdate.Object, false, "spec", "suspend"))
	_, err = jobsFor(kueueDynamic, config.namespace).Update(
		ctx, kueueUpdate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, err, "approved Kueue identity may change only suspension")
	_, err = jobsFor(operatorDynamic, config.namespace).Update(
		ctx, kueueUpdate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, expectDenied(err, "taugrid-nccl-rdma-job-boundary"))

	var typedJob batchv1.Job
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(createdJob.Object, &typedJob))
	jobControllerClient, err := kubernetes.NewForConfig(impersonated(restConfig, config.jobController))
	require.NoError(t, err)
	statusUpdate := typedJob.DeepCopy()
	statusUpdate.Status.Active = 1
	_, err = jobControllerClient.BatchV1().Jobs(config.namespace).UpdateStatus(
		ctx, statusUpdate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, err, "approved Job controller may update status")
	mutatedPod := generatedPodProbe(&typedJob, 1)
	mutatedPod.Spec.Volumes[1].Secret.SecretName = "attacker-secret"
	_, err = jobControllerClient.CoreV1().Pods(config.namespace).Create(
		ctx, mutatedPod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, expectDenied(err, "taugrid-nccl-rdma-generated-pods-boundary"))
	createdPod, err := jobControllerClient.CoreV1().Pods(config.namespace).Create(
		ctx, generatedPodProbe(&typedJob, 0), metav1.CreateOptions{},
	)
	require.NoError(t, err, "exact Job-controller Pod must pass local API admission")
	finalizerUpdate := createdPod.DeepCopy()
	finalizerUpdate.Finalizers = nil
	_, err = jobControllerClient.CoreV1().Pods(config.namespace).Update(
		ctx, finalizerUpdate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, err, "approved Job controller may remove only its tracking finalizer")

	untrustedClient, err := kubernetes.NewForConfig(impersonated(restConfig, config.untrusted))
	require.NoError(t, err)
	_, err = untrustedClient.CoreV1().Pods(config.namespace).Create(
		ctx, maliciousPodProbe(config.namespace),
		metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	require.NoError(t, expectDenied(err, "taugrid-nccl-rdma-generated-pods-boundary"))
	_, err = untrustedDynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).
		Namespace(config.namespace).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "attacker-secret",
			"namespace": config.namespace,
		},
		"stringData": map[string]interface{}{"token": "attacker"},
	}}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	require.NoError(t, expectDenied(err, "taugrid-nccl-rdma-secret-boundary"))
}

func TestNCCLRDMABoundaryReportsNoTypeCheckWarnings(t *testing.T) {
	kubeconfig := strings.TrimSpace(os.Getenv("NCCL_RDMA_TYPECHECK_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("set NCCL_RDMA_TYPECHECK_KUBECONFIG to a disposable Kubernetes cluster with a controller manager")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	ctx := context.Background()

	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: boundaryNamespace,
			Labels: map[string]string{
				"tau.azure.com/nccl-rdma-security-boundary": "v3",
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		propagation := metav1.DeletePropagationForeground
		require.NoError(t, client.CoreV1().Namespaces().Delete(
			context.Background(),
			boundaryNamespace,
			metav1.DeleteOptions{PropagationPolicy: &propagation},
		))
	})

	probe, err := e2e.ReadRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	require.NoError(t, err)
	expected := testBoundaryDocumentsWithProbe(t, string(probe))
	for _, policy := range expected.policies {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
			Create(ctx, policy.DeepCopy(), metav1.CreateOptions{})
		require.NoError(t, err)
		policyName := policy.Name
		t.Cleanup(func() {
			require.NoError(t, client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
				Delete(context.Background(), policyName, metav1.DeleteOptions{}))
		})
	}
	for _, binding := range expected.bindings {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
			Create(ctx, binding.DeepCopy(), metav1.CreateOptions{})
		require.NoError(t, err)
		bindingName := binding.Name
		t.Cleanup(func() {
			require.NoError(t, client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
				Delete(context.Background(), bindingName, metav1.DeleteOptions{}))
		})
	}

	require.NoError(t, wait.PollUntilContextTimeout(
		ctx, 250*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			for name := range expected.policies {
				policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
					Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
					return false, nil
				}
				if len(policy.Status.TypeChecking.ExpressionWarnings) != 0 {
					return false, fmt.Errorf(
						"ValidatingAdmissionPolicy %s has type-check warnings: %v",
						name,
						policy.Status.TypeChecking.ExpressionWarnings,
					)
				}
			}
			return true, nil
		},
	))
	require.NoError(t, validateActiveBoundary(ctx, client, expected))
}

func TestInteractiveBypassProbesSendNonPersistingSubresourceRequests(t *testing.T) {
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, metav1.DryRunAll, request.URL.Query().Get("dryRun"))
		requests[request.Method+" "+request.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, err := w.Write([]byte(`{
			"apiVersion":"v1",
			"kind":"Status",
			"status":"Failure",
			"reason":"Forbidden",
			"message":"taugrid-nccl-rdma-connect-deny denied request",
			"code":403
		}`))
		require.NoError(t, err)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)

	for _, subresource := range []string{"exec", "attach", "portforward"} {
		err := connectDryRun(context.Background(), client, boundaryNamespace, subresource)
		require.NoError(t, expectDenied(err, "taugrid-nccl-rdma-connect-deny"))
	}
	require.NoError(t, ephemeralContainerDryRun(context.Background(), client, boundaryNamespace))

	for _, suffix := range []string{"/exec", "/attach", "/portforward"} {
		key := "POST /api/v1/namespaces/" + boundaryNamespace +
			"/pods/e2e-nccl-rdma-policy-probe" + suffix
		require.Equal(t, 1, requests[key])
	}
	ephemeralKey := "PUT /api/v1/namespaces/" + boundaryNamespace +
		"/pods/e2e-nccl-rdma-policy-probe/ephemeralcontainers"
	require.Equal(t, 1, requests[ephemeralKey])
	for path := range requests {
		require.False(t, strings.Contains(path, "delete"))
	}
}

func testBoundaryDocuments(t *testing.T) boundaryDocuments {
	return testBoundaryDocumentsWithProbe(t, "print('probe')\n")
}

func testBoundaryDocumentsWithProbe(t *testing.T, probe string) boundaryDocuments {
	t.Helper()
	source, err := e2e.ReadRepoFile("tests/e2e/stack/fixtures/nccl-rdma-security-boundary.yaml")
	require.NoError(t, err)
	rendered, err := renderBoundary(source, probe, testCheckConfig())
	require.NoError(t, err)
	documents, err := decodeBoundaryDocuments(rendered)
	require.NoError(t, err)
	return documents
}

func fakeBoundaryClient(
	t *testing.T,
	expected boundaryDocuments,
	mutate func(kubernetes.Interface),
) kubernetes.Interface {
	t.Helper()
	client := fake.NewSimpleClientset()
	ctx := context.Background()
	for _, policy := range expected.policies {
		copy := policy.DeepCopy()
		copy.Generation = 1
		copy.Status.ObservedGeneration = 1
		copy.Status.TypeChecking = &admissionv1.TypeChecking{}
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().
			Create(ctx, copy, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	for _, binding := range expected.bindings {
		_, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
			Create(ctx, binding.DeepCopy(), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	if mutate != nil {
		mutate(client)
	}
	return client
}

func testCheckConfig() checkConfig {
	return checkConfig{
		kubeconfig:      "/tmp/kubeconfig",
		contextName:     "context",
		boundaryPath:    "/tmp/boundary",
		jobPath:         "/tmp/job",
		probePath:       "/tmp/probe",
		namespace:       boundaryNamespace,
		queue:           boundaryQueue,
		clusterQueue:    "owned-h200-rdma",
		selectorKey:     "accelerator",
		selectorValue:   "nvidia-h200",
		invocation:      "nccl-rdma-0123456789abcdef0123456789abcdef",
		operator:        "operator@example.com",
		kueueController: "system:serviceaccount:kueue-system:kueue-controller-manager",
		jobController:   "system:serviceaccount:kube-system:job-controller",
		untrusted:       "attacker@example.com",
		timeout:         time.Minute,
	}
}
