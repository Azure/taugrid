output "gpu_operator_release" {
  description = "Installed NVIDIA GPU Operator release."
  value = {
    name      = helm_release.gpu_operator.name
    namespace = helm_release.gpu_operator.namespace
    version   = helm_release.gpu_operator.version
  }
}

output "taugrid_dcgm_exporter_url" {
  description = "Cluster-local exporter URL to configure in TauGrid gpu-monitoring."
  value       = "http://nvidia-dcgm-exporter.${var.namespace}.svc:9400/metrics"
}
