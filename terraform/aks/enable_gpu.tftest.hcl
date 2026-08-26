mock_provider "azurerm" {}

mock_provider "local" {}

run "gpu_is_enabled_by_default" {
  command = plan

  variables {
    subscription_id  = "00000000-0000-0000-0000-000000000000"
    tenant_id        = "00000000-0000-0000-0000-000000000000"
    adx_cluster_name = ""
    enable_adx       = false
    install_taugrid  = false
  }

  assert {
    condition     = length(azurerm_kubernetes_cluster_node_pool.gpu) == 1
    error_message = "The default configuration must continue to create a GPU node pool."
  }

  assert {
    condition     = yamldecode(local.taugrid_values).components.gpuMonitoring.enabled
    error_message = "The default configuration must continue to enable GPU monitoring."
  }
}

run "cpu_only_omits_gpu_resources" {
  command = plan

  variables {
    subscription_id  = "00000000-0000-0000-0000-000000000000"
    tenant_id        = "00000000-0000-0000-0000-000000000000"
    adx_cluster_name = ""
    enable_adx       = false
    enable_gpu       = false
    install_taugrid  = false
  }

  assert {
    condition     = length(azurerm_kubernetes_cluster_node_pool.gpu) == 0
    error_message = "CPU-only deployments must not create a GPU node pool."
  }

  assert {
    condition     = !yamldecode(local.taugrid_values).baselineQueue.gpu.enabled && !yamldecode(local.taugrid_values).components.gpuMonitoring.enabled
    error_message = "CPU-only deployments must disable GPU admission and GPU monitoring."
  }

  assert {
    condition     = !can(yamldecode(local.taugrid_values)["gpu-monitoring"])
    error_message = "CPU-only deployments must not configure gpu-monitoring."
  }
}

run "cpu_only_keeps_optional_adx" {
  command = plan

  variables {
    subscription_id  = "00000000-0000-0000-0000-000000000000"
    tenant_id        = "00000000-0000-0000-0000-000000000000"
    adx_cluster_name = ""
    enable_adx       = true
    enable_gpu       = false
    install_taugrid  = false
  }

  assert {
    condition     = length(terraform_data.install_adx_mon) == 1 && length(local_file.adx_mon_values) == 1
    error_message = "CPU-only deployments must retain optional ADX monitoring when enable_adx is true."
  }
}
