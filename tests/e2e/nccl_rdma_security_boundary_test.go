// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/tests/e2e/internal/ncclimage"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	ncclRDMASecurityBoundaryFixture = "tests/e2e/stack/fixtures/nccl-rdma-security-boundary.yaml"
	ncclRDMABoundaryNamespace       = "taugrid-rdma-diagnostic"
	ncclRDMAOperatorUsername        = "APPROVED_OPERATOR_USERNAME"
	ncclRDMAKueueUsername           = "APPROVED_KUEUE_CONTROLLER_USERNAME"
)

func TestNCCLRDMASecurityBoundaryFixture(t *testing.T) {
	documents := decodeNCCLRDMABoundaryDocuments(t)
	require.Len(t, documents, 19)
	require.NotContains(t, documents, "Role/nccl-rdma-submitter")
	require.NotContains(t, documents, "RoleBinding/nccl-rdma-submitter")
	require.NotContains(t, documents, "ServiceAccount/nccl-rdma-runner")

	namespace := decodeNCCLRDMABoundaryDocument[corev1.Namespace](
		t, documents, "Namespace/taugrid-rdma-diagnostic",
	)
	require.Len(t, namespace.Labels, 7)
	require.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/enforce"])
	require.Equal(t, "latest", namespace.Labels["pod-security.kubernetes.io/enforce-version"])
	require.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/warn"])
	require.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/audit"])
	require.Equal(t, "v3", namespace.Labels["tau.azure.com/nccl-rdma-security-boundary"])
	require.Equal(t, map[string]string{
		"tau.azure.com/nccl-rdma-diagnostic-approved": "true",
		"tau.azure.com/owner-role":                    "tau-platform-admins",
	}, namespace.Annotations)
	require.Equal(t, "true", namespace.Annotations["tau.azure.com/nccl-rdma-diagnostic-approved"])

	quota := decodeNCCLRDMABoundaryDocument[corev1.ResourceQuota](
		t, documents, "ResourceQuota/nccl-rdma-shape",
	)
	podsQuota := quota.Spec.Hard[corev1.ResourcePods]
	gpuQuota := quota.Spec.Hard[corev1.ResourceName("requests.nvidia.com/gpu")]
	rdmaQuota := quota.Spec.Hard[corev1.ResourceName("requests.rdma/rdma_shared_device_a")]
	require.Equal(t, "2", podsQuota.String())
	require.Equal(t, "2", gpuQuota.String())
	require.Equal(t, "2", rdmaQuota.String())
	configMapQuota := quota.Spec.Hard[corev1.ResourceName("count/configmaps")]
	serviceAccountQuota := quota.Spec.Hard[corev1.ResourceName("count/serviceaccounts")]
	networkPolicyQuota := quota.Spec.Hard[corev1.ResourceName("count/networkpolicies.networking.k8s.io")]
	require.Equal(t, "2", configMapQuota.String())
	require.Equal(t, "2", serviceAccountQuota.String())
	require.Equal(t, "1", networkPolicyQuota.String())

	localQueue := decodeNCCLRDMABoundaryDocument[unstructured.Unstructured](
		t, documents, "LocalQueue/h200-rdma",
	)
	clusterQueue, found, err := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "APPROVED_OWNED_DIAGNOSTIC_CLUSTER_QUEUE", clusterQueue)

	jobPolicy := decodeNCCLRDMABoundaryDocument[admissionregistrationv1.ValidatingAdmissionPolicy](
		t, documents, "ValidatingAdmissionPolicy/taugrid-nccl-rdma-job-boundary",
	)
	require.Equal(t, admissionregistrationv1.Fail, *jobPolicy.Spec.FailurePolicy)
	require.Len(t, jobPolicy.Spec.MatchConstraints.ResourceRules, 1)
	require.Equal(t, []string{"batch"}, jobPolicy.Spec.MatchConstraints.ResourceRules[0].APIGroups)
	require.Equal(t, []string{"jobs", "jobs/status"}, jobPolicy.Spec.MatchConstraints.ResourceRules[0].Resources)
	allJobCEL := allNCCLRDMACEL(jobPolicy)
	require.NoError(t, validateNCCLRDMACELDelimiters(allJobCEL))
	require.Contains(t, allJobCEL, ncclimage.Image)
	for _, required := range []string{
		"APPROVED_OPERATOR_USERNAME",
		"APPROVED_KUEUE_CONTROLLER_USERNAME",
		"APPROVED_JOB_CONTROLLER_USERNAME",
		"APPROVED_GARBAGE_COLLECTOR_USERNAME",
		`object.spec.completionMode == "Indexed"`,
		`object.spec.completions == 2`,
		`object.spec.parallelism == 2`,
		`object.spec.manualSelector == true`,
		`object.spec.selector.matchLabels.size() == 3`,
		`object.spec.selector.matchLabels["e2e.taugrid.azure.com/invocation"]`,
		`object.spec.selector.matchLabels.all(`,
		`object.spec.template.metadata.labels.size() == 4`,
		`variables.pod.nodeName == ""`,
		`variables.container.command == ["/usr/local/bin/torchrun"]`,
		`"--node-rank=$(JOB_COMPLETION_INDEX)"`,
		`variables.container.env.size() == 15`,
		`fieldPath == "spec.nodeName"`,
		`metadata.annotations['batch.kubernetes.io/job-completion-index']`,
		`!has(variables.pod.preemptionPolicy)`,
		`variables.pod.enableServiceLinks == false`,
		`variables.container.securityContext.capabilities.add.size() == 0`,
		`variables.container.securityContext.readOnlyRootFilesystem == true`,
		`variables.pod.volumes.size() == 4`,
		`!has(volume.projected)`,
		`!has(volume.hostPath)`,
		`!has(volume.nfs)`,
		`mount.subPathExpr == ""`,
		`labelSelector.matchLabels.size() == 2`,
		`object.spec.template == oldObject.spec.template`,
	} {
		require.Contains(t, allJobCEL, required)
	}
	for _, forbidden := range []string{"MPIJob", "kubeflow.org", "sshd", "IPC_LOCK", "SYS_RESOURCE", "SETUID", "SETGID", "SYS_CHROOT"} {
		require.NotContains(t, allJobCEL, forbidden)
	}

	supportPolicies := map[string]string{
		"taugrid-nccl-rdma-serviceaccount-boundary": "serviceaccounts",
		"taugrid-nccl-rdma-configmap-boundary":      "configmaps",
		"taugrid-nccl-rdma-secret-boundary":         "secrets",
		"taugrid-nccl-rdma-service-boundary":        "services",
		"taugrid-nccl-rdma-networkpolicy-boundary":  "networkpolicies",
	}
	var supportCEL strings.Builder
	for policyName, resource := range supportPolicies {
		policy := decodeNCCLRDMABoundaryDocument[admissionregistrationv1.ValidatingAdmissionPolicy](
			t, documents, "ValidatingAdmissionPolicy/"+policyName,
		)
		require.Len(t, policy.Spec.MatchConstraints.ResourceRules, 1)
		require.Equal(t, []string{resource}, policy.Spec.MatchConstraints.ResourceRules[0].Resources)
		supportCEL.WriteString(allNCCLRDMACEL(policy))
	}
	allSupportCEL := supportCEL.String()
	require.NoError(t, validateNCCLRDMACELDelimiters(allSupportCEL))
	require.NotContains(t, allSupportCEL, "object.kind !=")
	for _, required := range []string{
		`object.metadata.name == "nccl-rdma-runner"`,
		`object.automountServiceAccountToken == false`,
		`APPROVED_TORCHRUN_RDMA_PROBE_EXACT`,
		`object.data["key"].size() == 32`,
		`"batch.kubernetes.io/job-completion-index"] == "0"`,
		`object.spec.selector.size() == 4`,
		`object.spec.clusterIP == "None"`,
		`object.spec.podSelector.matchLabels.size() == 3`,
		`object.spec.podSelector.matchExpressions.size() == 0`,
		`object.spec.ingress[0].from[0].podSelector.matchLabels.size() == 3`,
		`object.spec.egress[0].to[0].podSelector.matchLabels.size() == 3`,
		`port.protocol == "UDP" && port.port == 53`,
		`port.protocol == "TCP" && port.port == 53`,
		`"kube-system"`,
		`"kube-dns"`,
		`request.userInfo.username == "APPROVED_GARBAGE_COLLECTOR_USERNAME"`,
		`oldObject.metadata.finalizers == ["foregroundDeletion"]`,
	} {
		require.Contains(t, allSupportCEL, required)
	}
	require.NotContains(t, allSupportCEL, "podSelector ==")

	connectPolicy := decodeNCCLRDMABoundaryDocument[admissionregistrationv1.ValidatingAdmissionPolicy](
		t, documents, "ValidatingAdmissionPolicy/taugrid-nccl-rdma-connect-deny",
	)
	require.Equal(t, "false", connectPolicy.Spec.Validations[0].Expression)
	require.ElementsMatch(t, []string{"pods/exec", "pods/attach", "pods/portforward"},
		connectPolicy.Spec.MatchConstraints.ResourceRules[0].Resources)
	require.Equal(t, []string{"pods/ephemeralcontainers"},
		connectPolicy.Spec.MatchConstraints.ResourceRules[1].Resources)

	generatedPolicy := decodeNCCLRDMABoundaryDocument[admissionregistrationv1.ValidatingAdmissionPolicy](
		t, documents, "ValidatingAdmissionPolicy/taugrid-nccl-rdma-generated-pods-boundary",
	)
	allGeneratedPodCEL := allNCCLRDMACEL(generatedPolicy)
	require.NoError(t, validateNCCLRDMACELDelimiters(allGeneratedPodCEL))
	for _, required := range []string{
		`request.userInfo.username == "APPROVED_JOB_CONTROLLER_USERNAME"`,
		`object.metadata.labels.size() == 5`,
		`object.metadata.labels["batch.kubernetes.io/job-completion-index"] in ["0", "1"]`,
		`object.metadata.finalizers == ["batch.kubernetes.io/job-tracking"]`,
		`object.spec.nodeName == ""`,
		`object.spec.hostname ==`,
		`variables.container.image ==`,
		`object.spec.volumes[1].secret.secretName == "nccl-rdma-auth"`,
		`object.spec == oldObject.spec`,
	} {
		require.Contains(t, allGeneratedPodCEL, required)
	}

	for key := range documents {
		if !strings.HasPrefix(key, "ValidatingAdmissionPolicyBinding/") {
			continue
		}
		binding := decodeNCCLRDMABoundaryDocument[admissionregistrationv1.ValidatingAdmissionPolicyBinding](
			t, documents, key,
		)
		require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			binding.Spec.ValidationActions)
		require.Equal(t, "v3",
			binding.Spec.MatchResources.NamespaceSelector.MatchLabels["tau.azure.com/nccl-rdma-security-boundary"])
		require.Equal(t, map[string]string{
			"app.kubernetes.io/name":                    "nccl-rdma-diagnostic",
			"app.kubernetes.io/managed-by":              "platform-gitops",
			"tau.azure.com/nccl-rdma-security-boundary": "v3",
		}, binding.Labels)
	}
}

func TestNCCLRDMAJobBoundaryModelRejectsEveryBypass(t *testing.T) {
	base := renderNCCLRDMABoundaryJob(t)
	require.NoError(t, validateNCCLRDMAJobBoundary(base, base, "CREATE", ncclRDMAOperatorUsername))

	tests := map[string]func(*batchv1.Job){
		"name":       func(job *batchv1.Job) { job.Name = "other" },
		"namespace":  func(job *batchv1.Job) { job.Namespace = "default" },
		"top label":  func(job *batchv1.Job) { job.Labels["extra"] = "true" },
		"annotation": func(job *batchv1.Job) { job.Annotations = map[string]string{"sidecar.istio.io/inject": "true"} },
		"owner": func(job *batchv1.Job) {
			job.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: "attacker"}}
		},
		"unsuspended create": func(job *batchv1.Job) { job.Spec.Suspend = boolPointer(false) },
		"manual selector disabled": func(job *batchv1.Job) {
			job.Spec.ManualSelector = boolPointer(false)
		},
		"selector overlap without invocation": func(job *batchv1.Job) {
			delete(job.Spec.Selector.MatchLabels, NCCLRDMAInvocationKey)
		},
		"selector wrong invocation": func(job *batchv1.Job) {
			job.Spec.Selector.MatchLabels[NCCLRDMAInvocationKey] =
				"nccl-rdma-ffffffffffffffffffffffffffffffff"
		},
		"selector not represented in template": func(job *batchv1.Job) {
			job.Spec.Template.Labels["batch.kubernetes.io/job-name"] = "other"
		},
		"completion mode": func(job *batchv1.Job) {
			mode := batchv1.NonIndexedCompletion
			job.Spec.CompletionMode = &mode
		},
		"parallelism":    func(job *batchv1.Job) { job.Spec.Parallelism = int32Pointer(1) },
		"retry":          func(job *batchv1.Job) { job.Spec.BackoffLimit = int32Pointer(1) },
		"deadline":       func(job *batchv1.Job) { job.Spec.ActiveDeadlineSeconds = int64Pointer(901) },
		"template label": func(job *batchv1.Job) { job.Spec.Template.Labels["unexpected"] = "true" },
		"template annotation": func(job *batchv1.Job) {
			job.Spec.Template.Annotations = map[string]string{"azure.workload.identity/use": "true"}
		},
		"template finalizer": func(job *batchv1.Job) {
			job.Spec.Template.Finalizers = []string{"unexpected.example/finalizer"}
		},
		"service account": func(job *batchv1.Job) { job.Spec.Template.Spec.ServiceAccountName = "default" },
		"automount token": func(job *batchv1.Job) {
			job.Spec.Template.Spec.AutomountServiceAccountToken = boolPointer(true)
		},
		"nodeName":      func(job *batchv1.Job) { job.Spec.Template.Spec.NodeName = "attacker-selected-node" },
		"scheduler":     func(job *batchv1.Job) { job.Spec.Template.Spec.SchedulerName = "attacker-scheduler" },
		"priority":      func(job *batchv1.Job) { job.Spec.Template.Spec.PriorityClassName = "system-cluster-critical" },
		"runtime class": func(job *batchv1.Job) { job.Spec.Template.Spec.RuntimeClassName = strPointer("unexpected") },
		"host network":  func(job *batchv1.Job) { job.Spec.Template.Spec.HostNetwork = true },
		"host pid":      func(job *batchv1.Job) { job.Spec.Template.Spec.HostPID = true },
		"host ipc":      func(job *batchv1.Job) { job.Spec.Template.Spec.HostIPC = true },
		"image pull secret": func(job *batchv1.Job) {
			job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "credential"}}
		},
		"pod sysctl": func(job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext.Sysctls = []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"}}
		},
		"non-restricted UID": func(job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext.RunAsUser = int64Pointer(0)
		},
		"extra capability": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_RESOURCE"}
		},
		"writable root": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = boolPointer(false)
		},
		"image": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Image = "nvcr.io/nvidia/pytorch:25.11-py3"
		},
		"command": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "id"}
		},
		"args": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Args = []string{"--standalone"}
		},
		"env": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Env[0].Value = "gloo"
		},
		"envFrom": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].EnvFrom =
				[]corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credential"}}}}
		},
		"node identity fieldRef": func(job *batchv1.Job) {
			findJobEnv(job, "TAUGRID_NODE_NAME").ValueFrom.FieldRef.FieldPath = "metadata.name"
		},
		"completion index fieldRef": func(job *batchv1.Job) {
			findJobEnv(job, "JOB_COMPLETION_INDEX").ValueFrom.FieldRef.FieldPath = "metadata.name"
		},
		"preemption": func(job *batchv1.Job) {
			job.Spec.Template.Spec.PreemptionPolicy = func() *corev1.PreemptionPolicy {
				value := corev1.PreemptNever
				return &value
			}()
		},
		"dns config": func(job *batchv1.Job) {
			job.Spec.Template.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"192.0.2.53"}}
		},
		"supplemental group": func(job *batchv1.Job) {
			job.Spec.Template.Spec.SecurityContext.SupplementalGroups = []int64{0}
		},
		"supplemental group policy": func(job *batchv1.Job) {
			policy := corev1.SupplementalGroupsPolicyStrict
			job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy = &policy
		},
		"unconfined apparmor": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{
				Type: corev1.AppArmorProfileTypeUnconfined,
			}
		},
		"lifecycle exec": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
				PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "id"}}},
			}
		},
		"termination message path": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/tmp/result"
		},
		"host port": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Ports[0].HostPort = 29500
		},
		"resources": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")] =
				job.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName("cpu")]
		},
		"node selector": func(job *batchv1.Job) {
			job.Spec.Template.Spec.NodeSelector["accelerator"] = "other"
		},
		"toleration value": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Tolerations[0].Value = "unexpected"
		},
		"anti-affinity topology": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].
				TopologyKey = "topology.kubernetes.io/zone"
		},
		"anti-affinity label": func(job *batchv1.Job) {
			delete(job.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].
				LabelSelector.MatchLabels, NCCLRDMAInvocationKey)
		},
		"preferred affinity": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution =
				[]corev1.WeightedPodAffinityTerm{{Weight: 1}}
		},
		"node affinity": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		},
		"secret name": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[1].Secret.SecretName = "credential"
		},
		"projected token": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "token",
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
				}},
			})
		},
		"configmap": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[0].ConfigMap.Name = "attacker-payload"
		},
		"configmap item remap": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[0].ConfigMap.Items =
				[]corev1.KeyToPath{{Key: "torchrun-rdma-probe.py", Path: "other.py"}}
		},
		"optional secret": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes[1].Secret.Optional = boolPointer(true)
		},
		"nfs": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "nfs", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "192.0.2.1", Path: "/"}},
			})
		},
		"extra mount": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
				job.Spec.Template.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{Name: "auth", MountPath: "/credentials"},
			)
		},
		"subpath": func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[0].VolumeMounts[1].SubPath = "key"
		},
		"init container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "installer", Image: "busybox"}}
		},
		"ephemeral container": func(job *batchv1.Job) {
			job.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := base.DeepCopy()
			mutate(candidate)
			require.Error(t, validateNCCLRDMAJobBoundary(candidate, base, "CREATE", ncclRDMAOperatorUsername))
		})
	}
	require.Error(t, validateNCCLRDMAJobBoundary(base, base, "CREATE", "system:masters"))
}

func TestNCCLRDMAJobBoundaryAllowsOnlyControllerSuspendAndStatusUpdates(t *testing.T) {
	base := renderNCCLRDMABoundaryJob(t)
	update := base.DeepCopy()
	update.Spec.Suspend = boolPointer(false)
	update.Status.Succeeded = 2
	require.NoError(t, validateNCCLRDMAJobBoundary(update, base, "UPDATE", ncclRDMAKueueUsername))
	require.Error(t, validateNCCLRDMAJobBoundary(update, base, "UPDATE", ncclRDMAOperatorUsername))

	changed := update.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Args[0] = "--nnodes=3"
	require.Error(t, validateNCCLRDMAJobBoundary(changed, base, "UPDATE", ncclRDMAKueueUsername))
	require.Error(t, validateNCCLRDMAJobBoundary(update, base, "UPDATE", ncclRDMAOperatorUsername+"-other"))
}

func TestNCCLRDMASupportBoundaryModelRejectsEveryDeviation(t *testing.T) {
	const invocation = "nccl-rdma-0123456789abcdef0123456789abcdef"
	base, err := BuildNCCLRDMASupportResources(
		ncclRDMABoundaryNamespace,
		invocation,
		"print('approved probe')\n",
		make([]byte, 32),
	)
	require.NoError(t, err)

	for name, resource := range map[string]interface{}{
		"service account": base.ServiceAccount,
		"configmap":       base.ConfigMap,
		"secret":          base.Secret,
		"service":         base.Service,
		"network policy":  base.NetworkPolicy,
	} {
		t.Run(name+" baseline", func(t *testing.T) {
			require.NoError(t, validateNCCLRDMASupportBoundary(resource, resource))
		})
	}

	tests := map[string]struct {
		actual, expected interface{}
	}{
		"service account token automount": {
			mutateServiceAccount(base.ServiceAccount, func(value *corev1.ServiceAccount) {
				value.AutomountServiceAccountToken = boolPointer(true)
			}),
			base.ServiceAccount,
		},
		"service account image credential": {
			mutateServiceAccount(base.ServiceAccount, func(value *corev1.ServiceAccount) {
				value.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "credential"}}
			}),
			base.ServiceAccount,
		},
		"configmap mutable": {
			mutateConfigMap(base.ConfigMap, func(value *corev1.ConfigMap) {
				value.Immutable = boolPointer(false)
			}),
			base.ConfigMap,
		},
		"configmap payload": {
			mutateConfigMap(base.ConfigMap, func(value *corev1.ConfigMap) {
				value.Data["torchrun-rdma-probe.py"] = "print('attacker')\n"
			}),
			base.ConfigMap,
		},
		"configmap binary data": {
			mutateConfigMap(base.ConfigMap, func(value *corev1.ConfigMap) {
				value.BinaryData = map[string][]byte{"payload": {1}}
			}),
			base.ConfigMap,
		},
		"secret key length": {
			mutateSecret(base.Secret, func(value *corev1.Secret) {
				value.Data["key"] = make([]byte, 31)
			}),
			base.Secret,
		},
		"secret string data": {
			mutateSecret(base.Secret, func(value *corev1.Secret) {
				value.StringData = map[string]string{"key": "credential"}
			}),
			base.Secret,
		},
		"service selector widening": {
			mutateService(base.Service, func(value *corev1.Service) {
				delete(value.Spec.Selector, NCCLRDMAInvocationKey)
			}),
			base.Service,
		},
		"service external address": {
			mutateService(base.Service, func(value *corev1.Service) {
				value.Spec.ExternalIPs = []string{"192.0.2.10"}
			}),
			base.Service,
		},
		"service port": {
			mutateService(base.Service, func(value *corev1.Service) {
				value.Spec.Ports[0].Port = 443
			}),
			base.Service,
		},
		"network policy selector widening": {
			mutateNetworkPolicy(base.NetworkPolicy, func(value *networkingv1.NetworkPolicy) {
				delete(value.Spec.PodSelector.MatchLabels, NCCLRDMAInvocationKey)
			}),
			base.NetworkPolicy,
		},
		"network policy cross-namespace peer": {
			mutateNetworkPolicy(base.NetworkPolicy, func(value *networkingv1.NetworkPolicy) {
				value.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
			}),
			base.NetworkPolicy,
		},
		"network policy dns widening": {
			mutateNetworkPolicy(base.NetworkPolicy, func(value *networkingv1.NetworkPolicy) {
				value.Spec.Egress[1].Ports[0].Port = nil
			}),
			base.NetworkPolicy,
		},
		"support owner reference": {
			mutateSecret(base.Secret, func(value *corev1.Secret) {
				value.OwnerReferences = []metav1.OwnerReference{{Kind: "Pod", Name: "attacker"}}
			}),
			base.Secret,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateNCCLRDMASupportBoundary(test.actual, test.expected))
		})
	}
}

func validateNCCLRDMASupportBoundary(actual, expected interface{}) error {
	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		return errors.New("support resource kind changed")
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("support resource shape changed")
	}
	return nil
}

func mutateServiceAccount(
	source *corev1.ServiceAccount,
	mutate func(*corev1.ServiceAccount),
) *corev1.ServiceAccount {
	result := source.DeepCopy()
	mutate(result)
	return result
}

func mutateConfigMap(source *corev1.ConfigMap, mutate func(*corev1.ConfigMap)) *corev1.ConfigMap {
	result := source.DeepCopy()
	mutate(result)
	return result
}

func mutateSecret(source *corev1.Secret, mutate func(*corev1.Secret)) *corev1.Secret {
	result := source.DeepCopy()
	mutate(result)
	return result
}

func mutateService(source *corev1.Service, mutate func(*corev1.Service)) *corev1.Service {
	result := source.DeepCopy()
	mutate(result)
	return result
}

func mutateNetworkPolicy(
	source *networkingv1.NetworkPolicy,
	mutate func(*networkingv1.NetworkPolicy),
) *networkingv1.NetworkPolicy {
	result := source.DeepCopy()
	mutate(result)
	return result
}

func validateNCCLRDMAJobBoundary(
	job, expected *batchv1.Job,
	operation, username string,
) error {
	switch operation {
	case "CREATE":
		if username != ncclRDMAOperatorUsername {
			return errors.New("create identity is not approved")
		}
		if job.Spec.Suspend == nil || !*job.Spec.Suspend {
			return errors.New("created Job must be independently suspended")
		}
	case "UPDATE":
		if username != ncclRDMAKueueUsername && username != "APPROVED_JOB_CONTROLLER_USERNAME" {
			return errors.New("update identity is not approved")
		}
	default:
		return fmt.Errorf("unsupported operation %q", operation)
	}
	if job.Name != expected.Name ||
		job.Namespace != expected.Namespace ||
		!reflect.DeepEqual(job.Labels, expected.Labels) ||
		!reflect.DeepEqual(job.Annotations, expected.Annotations) ||
		!reflect.DeepEqual(job.Finalizers, expected.Finalizers) ||
		!reflect.DeepEqual(job.OwnerReferences, expected.OwnerReferences) {
		return errors.New("Job metadata contract mismatch")
	}
	actualSpec := job.Spec.DeepCopy()
	expectedSpec := expected.Spec.DeepCopy()
	actualSpec.Suspend = expectedSpec.Suspend
	if !reflect.DeepEqual(actualSpec, expectedSpec) {
		return errors.New("Job execution or security shape changed")
	}
	return nil
}

func renderNCCLRDMABoundaryJob(t *testing.T) *batchv1.Job {
	t.Helper()
	_, job := renderNCCLRDMAFixture(t)
	job.Namespace = ncclRDMABoundaryNamespace
	job.Labels["kueue.x-k8s.io/queue-name"] = "h200-rdma"
	job.Spec.Template.Spec.NodeSelector = map[string]string{"accelerator": "nvidia-h200"}
	return &job
}

func findJobEnv(job *batchv1.Job, name string) *corev1.EnvVar {
	for index := range job.Spec.Template.Spec.Containers[0].Env {
		if job.Spec.Template.Spec.Containers[0].Env[index].Name == name {
			return &job.Spec.Template.Spec.Containers[0].Env[index]
		}
	}
	return nil
}

func boolPointer(value bool) *bool    { return &value }
func int32Pointer(value int32) *int32 { return &value }
func int64Pointer(value int64) *int64 { return &value }
func strPointer(value string) *string { return &value }

func decodeNCCLRDMABoundaryDocuments(t *testing.T) map[string][]byte {
	t.Helper()
	data, err := ReadRepoFile(ncclRDMASecurityBoundaryFixture)
	require.NoError(t, err)

	documents := map[string][]byte{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var metadata struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		var raw map[string]interface{}
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
		}
		if len(raw) == 0 {
			continue
		}
		encoded, err := yaml.Marshal(raw)
		require.NoError(t, err)
		require.NoError(t, yaml.Unmarshal(encoded, &metadata))
		key := metadata.Kind + "/" + metadata.Metadata.Name
		require.NotContains(t, documents, key)
		documents[key] = encoded
	}
	return documents
}

func decodeNCCLRDMABoundaryDocument[T any](
	t *testing.T,
	documents map[string][]byte,
	key string,
) T {
	t.Helper()
	data, ok := documents[key]
	require.True(t, ok, "missing boundary document %s", key)
	var result T
	require.NoError(t, yaml.Unmarshal(data, &result))
	return result
}

func allNCCLRDMACEL(policy admissionregistrationv1.ValidatingAdmissionPolicy) string {
	expressions := make([]string, 0, len(policy.Spec.Variables)+len(policy.Spec.MatchConditions)+len(policy.Spec.Validations))
	for _, variable := range policy.Spec.Variables {
		expressions = append(expressions, variable.Expression)
	}
	for _, condition := range policy.Spec.MatchConditions {
		expressions = append(expressions, condition.Expression)
	}
	for _, validation := range policy.Spec.Validations {
		expressions = append(expressions, validation.Expression)
	}
	return strings.Join(expressions, "\n")
}

func validateNCCLRDMACELDelimiters(expression string) error {
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	stack := []rune{}
	var quote rune
	escaped := false
	for _, current := range expression {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == quote {
				quote = 0
			}
			continue
		}
		if current == '"' || current == '\'' {
			quote = current
			continue
		}
		switch current {
		case '(', '[', '{':
			stack = append(stack, current)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[current] {
				return fmt.Errorf("unbalanced CEL delimiter %q", current)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if quote != 0 || len(stack) != 0 {
		return errors.New("unterminated CEL string or delimiter")
	}
	return nil
}
