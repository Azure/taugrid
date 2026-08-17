resource "azurerm_resource_group" "this" {
  name     = var.resource_group_name
  location = var.location
  tags     = var.tags

  lifecycle {
    ignore_changes = [tags]
  }
}

resource "azurerm_kubernetes_cluster" "this" {
  name                = var.cluster_name
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  dns_prefix          = var.cluster_name
  kubernetes_version  = var.kubernetes_version
  sku_tier            = "Standard"

  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  default_node_pool {
    name                        = "system"
    vm_size                     = var.system_vm_size
    node_count                  = var.system_node_count
    temporary_name_for_rotation = "systmp"
    upgrade_settings {
      max_surge = "10%"
    }
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin    = "azure"
    load_balancer_sku = "standard"
  }

  tags = var.tags
}

resource "azurerm_kubernetes_cluster_node_pool" "gpu" {
  name                  = var.gpu_node_pool_name
  kubernetes_cluster_id = azurerm_kubernetes_cluster.this.id
  vm_size               = var.gpu_vm_size
  node_count            = var.gpu_node_count
  mode                  = "User"
  os_sku                = "Ubuntu"
  gpu_driver            = "Install"
  node_taints           = ["sku=gpu:NoSchedule"]
  node_labels = {
    "aks.azure.com/gpu-sku" = var.gpu_monitoring_sku_name
  }

  upgrade_settings {
    max_surge = "10%"
  }

  tags = var.tags
}

locals {
  generated_directory = "${path.module}/generated"
  kubeconfig_path     = "${local.generated_directory}/kubeconfig"
  values_path         = "${local.generated_directory}/taugrid-values.yaml"
  gpu_quota           = var.gpu_node_count * var.gpu_count_per_node
  taugrid_values = templatefile("${path.module}/taugrid-values.yaml.tftpl", {
    gpu_quota               = local.gpu_quota
    gpu_monitoring_sku_name = var.gpu_monitoring_sku_name
    gpu_vm_size             = var.gpu_vm_size
    gpu_count_per_node      = var.gpu_count_per_node
  })
}

resource "local_file" "taugrid_values" {
  filename        = local.values_path
  content         = local.taugrid_values
  file_permission = "0600"
}

resource "terraform_data" "install_taugrid" {
  count = var.install_taugrid ? 1 : 0

  triggers_replace = [
    azurerm_kubernetes_cluster_node_pool.gpu.id,
    local_file.taugrid_values.content_sha256,
    var.taugrid_version,
  ]

  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --resource-group ${azurerm_resource_group.this.name} --name ${azurerm_kubernetes_cluster.this.name} --file ${local.kubeconfig_path} --overwrite-existing; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; kubectl apply -f nvidia-device-plugin.yaml; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; tau cluster install --values ${local_file.taugrid_values.filename} --version ${var.taugrid_version} --timeout 20m"
  }

  depends_on = [
    local_file.taugrid_values,
    azurerm_kubernetes_cluster_node_pool.gpu,
  ]
}
