// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	NCCLRDMAJobName        = "e2e-nccl-rdma-2x1xh200"
	NCCLRDMAServiceAccount = "nccl-rdma-runner"
	NCCLRDMAConfigMap      = "nccl-rdma-probe"
	NCCLRDMASecret         = "nccl-rdma-auth"
	NCCLRDMAService        = "e2e-nccl-rdma-rank0"
	NCCLRDMANetworkPolicy  = "nccl-rdma-isolation"
	NCCLRDMAInvocationKey  = "e2e.taugrid.azure.com/invocation"
)

var ncclRDMAResourceInvocationRE = regexp.MustCompile(`^nccl-rdma-[a-f0-9]{32}$`)

type NCCLRDMASupportResources struct {
	ServiceAccount *corev1.ServiceAccount
	ConfigMap      *corev1.ConfigMap
	Secret         *corev1.Secret
	Service        *corev1.Service
	NetworkPolicy  *networkingv1.NetworkPolicy
}

func BuildNCCLRDMASupportResources(
	namespace, invocation, probeScript string,
	authKey []byte,
) (NCCLRDMASupportResources, error) {
	if namespace == "" {
		return NCCLRDMASupportResources{}, fmt.Errorf("namespace is required")
	}
	if !ncclRDMAResourceInvocationRE.MatchString(invocation) {
		return NCCLRDMASupportResources{}, fmt.Errorf("invocation must be nccl-rdma- followed by 32 lowercase hex characters")
	}
	if probeScript == "" {
		return NCCLRDMASupportResources{}, fmt.Errorf("probe script is required")
	}
	if len(authKey) != 32 {
		return NCCLRDMASupportResources{}, fmt.Errorf("authentication key must contain exactly 32 bytes")
	}

	labels := map[string]string{
		"app.kubernetes.io/name":           "nccl-rdma-diagnostic",
		"app.kubernetes.io/managed-by":     "taugrid-e2e",
		"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:              invocation,
	}
	podSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		"batch.kubernetes.io/job-name":     NCCLRDMAJobName,
		"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:              invocation,
	}}
	immutable := true
	protocolTCP := corev1.ProtocolTCP
	protocolUDP := corev1.ProtocolUDP
	rendezvousPort := intstr.FromInt32(29500)
	dnsPort := intstr.FromInt32(53)

	return NCCLRDMASupportResources{
		ServiceAccount: &corev1.ServiceAccount{
			TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta:                   metav1.ObjectMeta{Name: NCCLRDMAServiceAccount, Namespace: namespace, Labels: cloneStringMap(labels)},
			AutomountServiceAccountToken: boolPtr(false),
		},
		ConfigMap: &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: NCCLRDMAConfigMap, Namespace: namespace, Labels: cloneStringMap(labels)},
			Immutable:  &immutable,
			Data:       map[string]string{"torchrun-rdma-probe.py": probeScript},
		},
		Secret: &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: NCCLRDMASecret, Namespace: namespace, Labels: cloneStringMap(labels)},
			Immutable:  &immutable,
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"key": append([]byte(nil), authKey...)},
		},
		Service: &corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{Name: NCCLRDMAService, Namespace: namespace, Labels: cloneStringMap(labels)},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
				Selector: map[string]string{
					"batch.kubernetes.io/job-name":             NCCLRDMAJobName,
					"batch.kubernetes.io/job-completion-index": "0",
					"e2e.taugrid.azure.com/diagnostic":         "nccl-rdma-2x1xh200",
					NCCLRDMAInvocationKey:                      invocation,
				},
				Ports: []corev1.ServicePort{{
					Name:       "rendezvous",
					Protocol:   protocolTCP,
					Port:       29500,
					TargetPort: rendezvousPort,
				}},
			},
		},
		NetworkPolicy: &networkingv1.NetworkPolicy{
			TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
			ObjectMeta: metav1.ObjectMeta{Name: NCCLRDMANetworkPolicy, Namespace: namespace, Labels: cloneStringMap(labels)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: podSelector,
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress,
					networkingv1.PolicyTypeEgress,
				},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{PodSelector: &podSelector}},
				}},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &podSelector}}},
					{
						To: []networkingv1.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								"kubernetes.io/metadata.name": "kube-system",
							}},
							PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								"k8s-app": "kube-dns",
							}},
						}},
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &protocolUDP, Port: &dnsPort},
							{Protocol: &protocolTCP, Port: &dnsPort},
						},
					},
				},
			},
		},
	}, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func boolPtr(value bool) *bool {
	return &value
}
