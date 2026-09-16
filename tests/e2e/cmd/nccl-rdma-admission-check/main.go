// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	boundaryNamespace   = "taugrid-rdma-diagnostic"
	boundaryQueue       = "h200-rdma"
	boundaryPolicyCount = 8
)

var approvalPlaceholderRE = regexp.MustCompile(`APPROVED_[A-Z0-9_]+`)

type checkConfig struct {
	kubeconfig       string
	contextName      string
	boundaryPath     string
	jobPath          string
	probePath        string
	namespace        string
	queue            string
	clusterQueue     string
	selectorKey      string
	selectorValue    string
	invocation       string
	operator         string
	kueueController  string
	jobController    string
	garbageCollector string
	untrusted        string
	timeout          time.Duration
}

type boundaryDocuments struct {
	policies map[string]*admissionv1.ValidatingAdmissionPolicy
	bindings map[string]*admissionv1.ValidatingAdmissionPolicyBinding
}

func main() {
	var config checkConfig
	flag.StringVar(&config.kubeconfig, "kubeconfig", "", "explicit kubeconfig")
	flag.StringVar(&config.contextName, "context", "", "explicit kube context")
	flag.StringVar(&config.boundaryPath, "boundary", "", "security-boundary fixture")
	flag.StringVar(&config.jobPath, "job", "", "Indexed Job fixture")
	flag.StringVar(&config.probePath, "probe", "", "repository torchrun probe")
	flag.StringVar(&config.namespace, "namespace", "", "approved diagnostic namespace")
	flag.StringVar(&config.queue, "queue", "", "approved LocalQueue")
	flag.StringVar(&config.clusterQueue, "cluster-queue", "", "approved owned ClusterQueue")
	flag.StringVar(&config.selectorKey, "selector-key", "", "approved H200 selector key")
	flag.StringVar(&config.selectorValue, "selector-value", "", "approved H200 selector value")
	flag.StringVar(&config.invocation, "invocation", "", "unique diagnostic invocation")
	flag.StringVar(&config.operator, "operator", "", "approved operator username")
	flag.StringVar(&config.kueueController, "kueue-controller", "", "approved Kueue controller username")
	flag.StringVar(&config.jobController, "job-controller", "", "approved Job controller username")
	flag.StringVar(&config.garbageCollector, "garbage-collector", "", "approved garbage collector username")
	flag.StringVar(&config.untrusted, "untrusted", "", "explicit untrusted probe username")
	flag.DurationVar(&config.timeout, "timeout", 45*time.Second, "overall admission check timeout")
	flag.Parse()

	if err := run(context.Background(), config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(parent context.Context, config checkConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, config.timeout)
	defer cancel()

	boundarySource, err := os.ReadFile(config.boundaryPath)
	if err != nil {
		return err
	}
	probeSource, err := os.ReadFile(config.probePath)
	if err != nil {
		return err
	}
	renderedBoundary, err := renderBoundary(boundarySource, string(probeSource), config)
	if err != nil {
		return err
	}
	expected, err := decodeBoundaryDocuments(renderedBoundary)
	if err != nil {
		return err
	}
	baseConfig, err := explicitRESTConfig(config.kubeconfig, config.contextName)
	if err != nil {
		return err
	}
	baseClient, err := kubernetes.NewForConfig(baseConfig)
	if err != nil {
		return err
	}
	if err := validateActiveBoundary(ctx, baseClient, expected); err != nil {
		return err
	}
	return runAdmissionProbes(ctx, baseConfig, config, probeSource)
}

func (config checkConfig) validate() error {
	required := map[string]string{
		"kubeconfig": config.kubeconfig, "context": config.contextName,
		"boundary": config.boundaryPath, "job": config.jobPath, "probe": config.probePath,
		"namespace": config.namespace, "queue": config.queue, "cluster-queue": config.clusterQueue,
		"selector-key": config.selectorKey, "selector-value": config.selectorValue,
		"invocation": config.invocation, "operator": config.operator,
		"kueue-controller": config.kueueController, "job-controller": config.jobController,
		"garbage-collector": config.garbageCollector,
		"untrusted":         config.untrusted,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" || strings.Contains(value, "APPROVED_") {
			return fmt.Errorf("--%s must be explicit and cannot contain an unresolved approval placeholder", name)
		}
	}
	if config.namespace != boundaryNamespace || config.queue != boundaryQueue {
		return fmt.Errorf("security boundary is fixed to namespace %s and LocalQueue %s", boundaryNamespace, boundaryQueue)
	}
	identities := map[string]struct{}{}
	for _, identity := range []string{
		config.operator, config.kueueController, config.jobController, config.garbageCollector, config.untrusted,
	} {
		identities[identity] = struct{}{}
	}
	if len(identities) != 5 {
		return errors.New("operator, Kueue controller, Job controller, garbage collector, and untrusted probe identities must be distinct")
	}
	if config.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

func renderBoundary(source []byte, probe string, config checkConfig) ([]byte, error) {
	replacements := map[string]string{
		"APPROVED_RUN_INVOCATION":                 config.invocation,
		"APPROVED_OWNED_DIAGNOSTIC_CLUSTER_QUEUE": config.clusterQueue,
		"APPROVED_OPERATOR_USERNAME":              config.operator,
		"APPROVED_KUEUE_CONTROLLER_USERNAME":      config.kueueController,
		"APPROVED_JOB_CONTROLLER_USERNAME":        config.jobController,
		"APPROVED_GARBAGE_COLLECTOR_USERNAME":     config.garbageCollector,
		"APPROVED_H200_SELECTOR_KEY":              config.selectorKey,
		"APPROVED_H200_SELECTOR_VALUE":            config.selectorValue,
		`"APPROVED_TORCHRUN_RDMA_PROBE_EXACT"`:    fmt.Sprintf("%q", probe),
	}
	rendered := string(source)
	for placeholder, value := range replacements {
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	if approvalPlaceholderRE.MatchString(rendered) || strings.Contains(rendered, "{{") || strings.Contains(rendered, "}}") {
		return nil, errors.New("security boundary contains an unresolved identity or approval placeholder")
	}
	return []byte(rendered), nil
}

func decodeBoundaryDocuments(source []byte) (boundaryDocuments, error) {
	result := boundaryDocuments{
		policies: map[string]*admissionv1.ValidatingAdmissionPolicy{},
		bindings: map[string]*admissionv1.ValidatingAdmissionPolicyBinding{},
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(source), 4096)
	for {
		raw := &unstructured.Unstructured{}
		if err := decoder.Decode(raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, err
		}
		if raw.GetKind() == "" {
			continue
		}
		switch raw.GetKind() {
		case "ValidatingAdmissionPolicy":
			var policy admissionv1.ValidatingAdmissionPolicy
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, &policy); err != nil {
				return result, err
			}
			scheme.Scheme.Default(&policy)
			result.policies[policy.Name] = &policy
		case "ValidatingAdmissionPolicyBinding":
			var binding admissionv1.ValidatingAdmissionPolicyBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, &binding); err != nil {
				return result, err
			}
			scheme.Scheme.Default(&binding)
			result.bindings[binding.Name] = &binding
		}
	}
	if len(result.policies) != boundaryPolicyCount || len(result.bindings) != boundaryPolicyCount {
		return result, fmt.Errorf(
			"security boundary must render exactly %d policies and %d Deny bindings",
			boundaryPolicyCount,
			boundaryPolicyCount,
		)
	}
	return result, nil
}

func validateActiveBoundary(ctx context.Context, client kubernetes.Interface, expected boundaryDocuments) error {
	for name, wanted := range expected.policies {
		actual, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("required ValidatingAdmissionPolicy %s is not active: %w", name, err)
		}
		if !reflect.DeepEqual(actual.Labels, wanted.Labels) || !reflect.DeepEqual(actual.Annotations, wanted.Annotations) ||
			!reflect.DeepEqual(canonicalPolicySpec(actual.Spec), canonicalPolicySpec(wanted.Spec)) {
			return fmt.Errorf("active ValidatingAdmissionPolicy %s drifts from the rendered repository boundary", name)
		}
		if actual.Status.ObservedGeneration != actual.Generation || actual.Status.TypeChecking == nil {
			return fmt.Errorf("ValidatingAdmissionPolicy %s has not completed API-server type checking for its active generation", name)
		}
		if len(actual.Status.TypeChecking.ExpressionWarnings) != 0 {
			return fmt.Errorf("ValidatingAdmissionPolicy %s has API-server type-check warnings", name)
		}
	}
	for name, wanted := range expected.bindings {
		actual, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("required ValidatingAdmissionPolicyBinding %s is not active: %w", name, err)
		}
		if !reflect.DeepEqual(actual.Labels, wanted.Labels) || !reflect.DeepEqual(actual.Annotations, wanted.Annotations) ||
			!reflect.DeepEqual(canonicalBindingSpec(actual.Spec), canonicalBindingSpec(wanted.Spec)) {
			return fmt.Errorf("active ValidatingAdmissionPolicyBinding %s drifts from the rendered repository boundary", name)
		}
		if !reflect.DeepEqual(actual.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny}) {
			return fmt.Errorf("ValidatingAdmissionPolicyBinding %s is not enforcing Deny", name)
		}
	}
	return nil
}

func canonicalPolicySpec(spec admissionv1.ValidatingAdmissionPolicySpec) admissionv1.ValidatingAdmissionPolicySpec {
	copy := spec.DeepCopy()
	canonicalMatchResources(copy.MatchConstraints)
	return *copy
}

func canonicalBindingSpec(spec admissionv1.ValidatingAdmissionPolicyBindingSpec) admissionv1.ValidatingAdmissionPolicyBindingSpec {
	copy := spec.DeepCopy()
	canonicalMatchResources(copy.MatchResources)
	return *copy
}

func canonicalMatchResources(match *admissionv1.MatchResources) {
	if match == nil {
		return
	}
	// Kubernetes 1.34 stores omitted all-resource selectors and scope explicitly.
	// Canonicalize only those documented defaults so any narrowing or opt-in
	// selector still fails the active-boundary comparison.
	match.NamespaceSelector = canonicalLabelSelector(match.NamespaceSelector)
	match.ObjectSelector = canonicalLabelSelector(match.ObjectSelector)
	for index := range match.ResourceRules {
		canonicalRuleScope(&match.ResourceRules[index])
	}
	for index := range match.ExcludeResourceRules {
		canonicalRuleScope(&match.ExcludeResourceRules[index])
	}
}

func canonicalLabelSelector(selector *metav1.LabelSelector) *metav1.LabelSelector {
	if selector == nil || (len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0) {
		return &metav1.LabelSelector{}
	}
	return selector
}

func canonicalRuleScope(rule *admissionv1.NamedRuleWithOperations) {
	if rule.Scope == nil {
		scope := admissionv1.AllScopes
		rule.Scope = &scope
	}
}

func explicitRESTConfig(kubeconfig, contextName string) (*rest.Config, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func impersonated(config *rest.Config, username string) *rest.Config {
	copy := rest.CopyConfig(config)
	copy.Impersonate.UserName = username
	return copy
}

func runAdmissionProbes(ctx context.Context, base *rest.Config, config checkConfig, probeSource []byte) error {
	jobSource, err := os.ReadFile(config.jobPath)
	if err != nil {
		return err
	}
	job, err := renderJob(jobSource, config)
	if err != nil {
		return err
	}
	unstructured.RemoveNestedField(job.Object, "spec", "suspend")

	operatorDynamic, err := dynamic.NewForConfig(impersonated(base, config.operator))
	if err != nil {
		return err
	}
	jobs := operatorDynamic.Resource(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}).
		Namespace(config.namespace)
	admitted, err := jobs.Create(ctx, job, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return fmt.Errorf("approved operator exact Job dry-run was denied: %w", err)
	}
	suspended, found, err := unstructured.NestedBool(admitted.Object, "spec", "suspend")
	if err != nil || !found || !suspended {
		return errors.New("Kueue admission dry-run did not restore spec.suspend=true")
	}

	support, err := e2e.BuildNCCLRDMASupportResources(config.namespace, config.invocation, string(probeSource), make([]byte, 32))
	if err != nil {
		return err
	}
	if err := dryRunSupportCreates(ctx, operatorDynamic, config.namespace, support); err != nil {
		return err
	}

	return runDeniedAdmissionProbes(ctx, base, config, job)
}

func renderJob(source []byte, config checkConfig) (*unstructured.Unstructured, error) {
	renderedJob := string(source)
	for placeholder, value := range map[string]string{
		"{{STACK_NAMESPACE}}":         config.namespace,
		"{{STACK_LARGE_GPU_QUEUE}}":   config.queue,
		"{{NCCL_RDMA_INVOCATION}}":    config.invocation,
		"{{GPU_NODE_SELECTOR_KEY}}":   config.selectorKey,
		"{{GPU_NODE_SELECTOR_VALUE}}": config.selectorValue,
	} {
		renderedJob = strings.ReplaceAll(renderedJob, placeholder, value)
	}
	job := &unstructured.Unstructured{}
	if err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(renderedJob), 4096).Decode(job); err != nil {
		return nil, err
	}
	if approvalPlaceholderRE.MatchString(renderedJob) || strings.Contains(renderedJob, "{{") {
		return nil, errors.New("Job fixture contains an unresolved placeholder")
	}
	return job, nil
}

func runDeniedAdmissionProbes(
	ctx context.Context,
	base *rest.Config,
	config checkConfig,
	job *unstructured.Unstructured,
) error {
	untrustedDynamic, err := dynamic.NewForConfig(impersonated(base, config.untrusted))
	if err != nil {
		return err
	}
	maliciousJob := job.DeepCopy()
	containers, _, err := unstructured.NestedSlice(maliciousJob.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return err
	}
	containers[0].(map[string]interface{})["command"] = []interface{}{"/bin/sh", "-c", "id"}
	if err := unstructured.SetNestedSlice(maliciousJob.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return err
	}
	_, err = jobsFor(untrustedDynamic, config.namespace).Create(
		ctx, maliciousJob, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	if err := expectDenied(err, "taugrid-nccl-rdma-job-boundary"); err != nil {
		return fmt.Errorf("malicious Job dry-run: %w", err)
	}

	untrustedClient, err := kubernetes.NewForConfig(impersonated(base, config.untrusted))
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "attacker-secret", Namespace: config.namespace},
		StringData: map[string]string{"token": "attacker"},
	}
	_, err = untrustedClient.CoreV1().Secrets(config.namespace).Create(
		ctx, secret, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	if err := expectDenied(err, "taugrid-nccl-rdma-secret-boundary"); err != nil {
		return fmt.Errorf("malicious Secret dry-run: %w", err)
	}
	rootCA, err := untrustedClient.CoreV1().ConfigMaps(config.namespace).
		Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read kube-root-ca.crt for non-persisting support UPDATE probe: %w", err)
	}
	rootCA = rootCA.DeepCopy()
	if rootCA.Data == nil {
		rootCA.Data = map[string]string{}
	}
	rootCA.Data["attacker"] = "mutation"
	_, err = untrustedClient.CoreV1().ConfigMaps(config.namespace).Update(
		ctx, rootCA, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	if err := expectDenied(err, "taugrid-nccl-rdma-configmap-boundary"); err != nil {
		return fmt.Errorf("malicious support UPDATE dry-run: %w", err)
	}
	pod := maliciousPodProbe(config.namespace)
	_, err = untrustedClient.CoreV1().Pods(config.namespace).Create(
		ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	if err := expectDenied(err, "taugrid-nccl-rdma-generated-pods-boundary"); err != nil {
		return fmt.Errorf("malicious Pod/Secret mount dry-run: %w", err)
	}

	for _, subresource := range []string{"exec", "attach", "portforward"} {
		err := connectDryRun(ctx, untrustedClient, config.namespace, subresource)
		if denyErr := expectDenied(err, "taugrid-nccl-rdma-connect-deny"); denyErr != nil {
			return fmt.Errorf("%s bypass dry-run: %w", subresource, denyErr)
		}
	}
	return nil
}

func jobsFor(client dynamic.Interface, namespace string) dynamic.ResourceInterface {
	return client.Resource(schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}).
		Namespace(namespace)
}

func dryRunSupportCreates(
	ctx context.Context,
	client dynamic.Interface,
	namespace string,
	support e2e.NCCLRDMASupportResources,
) error {
	objects := []struct {
		gvr    schema.GroupVersionResource
		object any
	}{
		{schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, support.ServiceAccount},
		{schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, support.ConfigMap},
		{schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, support.Secret},
		{schema.GroupVersionResource{Version: "v1", Resource: "services"}, support.Service},
		{schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, support.NetworkPolicy},
	}
	for _, candidate := range objects {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(candidate.object)
		if err != nil {
			return err
		}
		object := &unstructured.Unstructured{Object: raw}
		if _, err := client.Resource(candidate.gvr).Namespace(namespace).Create(
			ctx, object, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
		); err != nil {
			return fmt.Errorf("approved operator exact %s/%s dry-run was denied: %w",
				candidate.gvr.Resource, object.GetName(), err)
		}
	}
	return nil
}

func generatedPodProbe(job *batchv1.Job, index int) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%d-", job.Name, index),
			Namespace:    job.Namespace,
			Labels:       map[string]string{},
			Annotations: map[string]string{
				batchv1.JobCompletionIndexAnnotation: fmt.Sprint(index),
			},
			Finalizers: []string{batchv1.JobTrackingFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               job.Name,
				UID:                types.UID("00000000-0000-4000-8000-000000000001"),
				Controller:         boolPointer(true),
				BlockOwnerDeletion: boolPointer(true),
			}},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	for key, value := range job.Spec.Template.Labels {
		pod.Labels[key] = value
	}
	pod.Labels[batchv1.JobCompletionIndexAnnotation] = fmt.Sprint(index)
	pod.Spec.Hostname = fmt.Sprintf("%s-%d", job.Name, index)
	return pod
}

func maliciousPodProbe(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-nccl-rdma-policy-probe", Namespace: namespace},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: boolPointer(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: boolPointer(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:         "attacker",
				Image:        "invalid.example/attacker@sha256:" + strings.Repeat("a", 64),
				Command:      []string{"/bin/false"},
				VolumeMounts: []corev1.VolumeMount{{Name: "credential", MountPath: "/credential"}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolPointer(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "credential",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: "attacker-secret",
				}},
			}},
		},
	}
}

func expectDenied(err error, policy string) error {
	if err == nil {
		return fmt.Errorf("request unexpectedly succeeded instead of being denied by %s", policy)
	}
	if (!apierrors.IsForbidden(err) && !strings.Contains(strings.ToLower(err.Error()), "forbidden")) ||
		!strings.Contains(err.Error(), policy) {
		return fmt.Errorf("request was not denied by %s: %w", policy, err)
	}
	return nil
}

func connectDryRun(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, subresource string,
) error {
	request := client.CoreV1().RESTClient().Post().
		Namespace(namespace).
		Resource("pods").
		Name("e2e-nccl-rdma-policy-probe").
		SubResource(subresource).
		Param("dryRun", metav1.DryRunAll)
	switch subresource {
	case "exec":
		request.VersionedParams(&corev1.PodExecOptions{
			Container: "probe",
			Command:   []string{"/bin/true"},
			Stdout:    true,
		}, scheme.ParameterCodec)
	case "attach":
		request.VersionedParams(&corev1.PodAttachOptions{
			Container: "probe",
			Stdout:    true,
		}, scheme.ParameterCodec)
	case "portforward":
		request.VersionedParams(&corev1.PodPortForwardOptions{
			Ports: []int32{29500},
		}, scheme.ParameterCodec)
	default:
		return fmt.Errorf("unsupported connect subresource %q", subresource)
	}
	return request.Do(ctx).Error()
}

func boolPointer(value bool) *bool {
	return &value
}
