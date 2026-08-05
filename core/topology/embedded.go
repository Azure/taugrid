package topology

import _ "embed"

const embeddedPolicySource = "embedded:azure-topology-policy.yaml"

//go:embed assets/azure-topology-policy.yaml
var embeddedDefaultPolicy []byte
