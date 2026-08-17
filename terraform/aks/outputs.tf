output "resource_group_name" {
  description = "Resource group containing the AKS environment."
  value       = azurerm_resource_group.this.name
}

output "cluster_name" {
  description = "AKS cluster name."
  value       = azurerm_kubernetes_cluster.this.name
}

output "get_credentials_command" {
  description = "Fetch an operator kubeconfig with Azure CLI."
  value       = "az aks get-credentials --resource-group ${azurerm_resource_group.this.name} --name ${azurerm_kubernetes_cluster.this.name} --overwrite-existing"
}

output "portal_port_forward_command" {
  description = "Operator-only Portal access command. A production browser endpoint needs an authenticated HTTPS proxy."
  value       = "kubectl -n tau port-forward service/tau-portal 18080:80"
}

output "adx_endpoint" {
  description = "Azure Data Explorer endpoint when enable_adx is true."
  value       = var.enable_adx ? azurerm_kusto_cluster.this[0].uri : null
}

output "gpu_node_pool_name" {
  description = "GPU workload node pool."
  value       = azurerm_kubernetes_cluster_node_pool.gpu.name
}

output "gpu_quota" {
  description = "GPU quota configured in TauGrid's baseline Kueue queue."
  value       = local.gpu_quota
}
