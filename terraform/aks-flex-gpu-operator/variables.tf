variable "kubeconfig_path" {
  description = "Path to the kubeconfig containing the existing AKS cluster."
  type        = string
  default     = "~/.kube/config"
}

variable "kube_context" {
  description = "Exact kubeconfig context for the AKS cluster with attached Flex GPU nodes."
  type        = string

  validation {
    condition     = length(trimspace(var.kube_context)) > 0
    error_message = "kube_context must name an existing AKS kubeconfig context."
  }
}

variable "gpu_operator_version" {
  description = "Pinned NVIDIA GPU Operator Helm chart version."
  type        = string
  default     = "v26.3.0"
}

variable "namespace" {
  description = "Namespace for the cluster-scoped NVIDIA GPU Operator release."
  type        = string
  default     = "gpu-operator"
}

variable "mig_strategy" {
  description = "GPU Operator MIG strategy. Keep none unless the platform has an explicit MIG partitioning plan."
  type        = string
  default     = "none"

  validation {
    condition     = contains(["none", "single", "mixed"], var.mig_strategy)
    error_message = "mig_strategy must be none, single, or mixed."
  }
}

variable "mig_manager_enabled" {
  description = "Enable GPU Operator MIG Manager. This is disruptive and remains off by default."
  type        = bool
  default     = false
}

variable "toolkit_enabled" {
  description = "Whether GPU Operator owns NVIDIA Container Toolkit configuration on target nodes."
  type        = bool
  default     = true
}

variable "toolkit_ownership_reviewed" {
  description = "Required acknowledgement that container toolkit/runtime ownership was reviewed for every target node."
  type        = bool
  default     = false

  validation {
    condition     = var.toolkit_ownership_reviewed
    error_message = "Set toolkit_ownership_reviewed=true only after deciding whether GPU Operator or the node image owns NVIDIA Container Toolkit configuration."
  }
}

variable "host_drivers_preinstalled" {
  description = "Required acknowledgement that the node-image owner provides a working host NVIDIA driver. This example never installs a driver."
  type        = bool
  default     = false

  validation {
    condition     = var.host_drivers_preinstalled
    error_message = "Set host_drivers_preinstalled=true only after verifying nvidia-smi on every target Flex GPU node."
  }
}
