output "azurefile_csi_release" {
  description = "Installed customer-managed Azure File CSI Driver release."
  value = {
    name        = helm_release.azurefile_csi.name
    namespace   = helm_release.azurefile_csi.namespace
    version     = helm_release.azurefile_csi.version
    driver_name = var.driver_name
  }
}

output "storage_class_name" {
  description = "Created Flex StorageClass name, or null when StorageClass creation is disabled."
  value       = var.create_storage_class ? kubernetes_storage_class_v1.azurefile_flex[0].metadata[0].name : null
}

output "validation_commands" {
  description = "Commands to verify node registration after apply."
  value = [
    "kubectl --context ${var.kube_context} get csidriver ${var.driver_name}",
    "kubectl --context ${var.kube_context} get daemonset ${local.linux_ds_name} -n ${var.namespace} -o wide",
    "kubectl --context ${var.kube_context} get csinode -o jsonpath='{range .items[*]}{.metadata.name}{\" \"}{range .spec.drivers[*]}{.name}{\",\"}{end}{\"\\n\"}{end}'",
  ]
}
