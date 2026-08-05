package kvspec

import (
	"bytes"
	"fmt"
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
)

// RenderSPC renders a SecretProviderClass YAML document.
func RenderSPC(name, namespace string, spec *Spec) ([]byte, error) {
	if spec == nil || len(spec.Entries) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, "apiVersion: secrets-store.csi.x-k8s.io/v1")
	fmt.Fprintln(&buf, "kind: SecretProviderClass")
	fmt.Fprintln(&buf, "metadata:")
	fmt.Fprintf(&buf, "  name: %s\n", name)
	fmt.Fprintf(&buf, "  namespace: %s\n", namespace)
	fmt.Fprintln(&buf, "  labels:")
	fmt.Fprintf(&buf, "    %s: %s\n", workloadmeta.LabelManagedBy, workloadmeta.ManagedByValue)
	fmt.Fprintln(&buf, "spec:")
	fmt.Fprintln(&buf, "  provider: azure")
	fmt.Fprintln(&buf, "  secretObjects:")

	syncName := SyncedSecretName(strings.TrimSuffix(name, "-kv"))
	fmt.Fprintf(&buf, "  - secretName: %s\n", syncName)
	fmt.Fprintln(&buf, "    type: Opaque")
	fmt.Fprintln(&buf, "    data:")
	for _, e := range spec.Entries {
		fmt.Fprintf(&buf, "    - key: %s\n", e.EnvVar)
		fmt.Fprintf(&buf, "      objectName: %s\n", e.SecretName)
	}

	fmt.Fprintln(&buf, "  parameters:")
	fmt.Fprintln(&buf, `    usePodIdentity: "false"`)
	fmt.Fprintln(&buf, `    useVMManagedIdentity: "false"`)
	fmt.Fprintf(&buf, "    clientID: %s\n", spec.ClientID)
	fmt.Fprintf(&buf, "    keyvaultName: %s\n", spec.Vault)
	fmt.Fprintf(&buf, "    tenantId: %s\n", spec.TenantID)
	fmt.Fprintln(&buf, "    objects: |")
	fmt.Fprintln(&buf, "      array:")
	for _, e := range spec.Entries {
		fmt.Fprintln(&buf, "        - |")
		fmt.Fprintf(&buf, "          objectName: %s\n", e.SecretName)
		fmt.Fprintln(&buf, "          objectType: secret")
	}
	return buf.Bytes(), nil
}

// VolumeYAML returns the CSI volume block for embedding in a pod spec at the
// given indent level (number of leading spaces before the "- name:" line).
func VolumeYAML(spcName string, indent int) string {
	pad := strings.Repeat(" ", indent)
	inner := strings.Repeat(" ", indent+2)
	return fmt.Sprintf("%s- name: kv-secrets\n%scsi:\n%s  driver: secrets-store.csi.k8s.io\n%s  readOnly: true\n%s  volumeAttributes:\n%s    secretProviderClass: %s",
		pad, inner, inner, inner, inner, inner, spcName)
}

// VolumeMountYAML returns the volume mount block for containers at the given
// indent level.
func VolumeMountYAML(indent int) string {
	pad := strings.Repeat(" ", indent)
	inner := strings.Repeat(" ", indent+2)
	return fmt.Sprintf("%s- name: kv-secrets\n%smountPath: /mnt/secrets-store\n%sreadOnly: true",
		pad, inner, inner)
}
