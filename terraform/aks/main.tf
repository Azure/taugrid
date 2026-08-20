data "azurerm_client_config" "current" {}

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

  dynamic "azure_active_directory_role_based_access_control" {
    for_each = length(var.aks_admin_group_object_ids) > 0 ? [true] : []

    content {
      azure_rbac_enabled     = var.azure_rbac_enabled
      tenant_id              = coalesce(var.tenant_id, data.azurerm_client_config.current.tenant_id)
      admin_group_object_ids = var.aks_admin_group_object_ids
    }
  }

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
  auto_scaling_enabled  = var.gpu_auto_scaling_enabled
  node_count            = var.gpu_auto_scaling_enabled ? null : var.gpu_node_count
  min_count             = var.gpu_auto_scaling_enabled ? var.gpu_min_count : null
  max_count             = var.gpu_auto_scaling_enabled ? var.gpu_max_count : null
  mode                  = "User"
  os_sku                = "Ubuntu"
  gpu_driver            = "Install"
  node_taints           = ["sku=gpu:NoSchedule"]
  node_labels = {
    "aks.azure.com/gpu-sku"      = var.gpu_monitoring_sku_name
    "tau.azure.com/gpu-class"    = var.gpu_class
    "kueue.azure.com/gpu-series" = var.gpu_series
  }

  upgrade_settings {
    max_surge = "10%"
  }

  tags = var.tags

  lifecycle {
    precondition {
      condition     = !var.normalize_gpu_mig || !var.gpu_auto_scaling_enabled
      error_message = "gpu_auto_scaling_enabled cannot be used with normalize_gpu_mig because newly scaled A100 nodes would not be normalized. Set normalize_gpu_mig=false only after validating the selected GPU SKU does not require MIG normalization."
    }
  }
}

locals {
  generated_directory = "${path.module}/generated"
  kubeconfig_path     = "${local.generated_directory}/kubeconfig"
  values_path         = "${local.generated_directory}/taugrid-values.yaml"
  gpu_quota           = (var.gpu_auto_scaling_enabled ? var.gpu_max_count : var.gpu_node_count) * var.gpu_count_per_node
  adx_databases       = toset(["Metrics", "Logs", "CostTracking", "Audit"])
  taugrid_values = templatefile("${path.module}/taugrid-values.yaml.tftpl", {
    gpu_quota                    = local.gpu_quota
    gpu_monitoring_sku_name      = var.gpu_monitoring_sku_name
    gpu_flavor_name              = var.gpu_flavor_name
    gpu_class                    = var.gpu_class
    gpu_series                   = var.gpu_series
    gpu_vm_size                  = var.gpu_vm_size
    gpu_count_per_node           = var.gpu_count_per_node
    adx_enabled                  = var.enable_adx
    lifecycle_recorder_enabled   = var.enable_lifecycle_recorder
    lifecycle_recorder_client_id = var.enable_lifecycle_recorder ? azurerm_user_assigned_identity.lifecycle_recorder[0].client_id : ""
    workspace_namespace          = var.workspace_namespace
    adx_endpoint                 = var.enable_adx ? azurerm_kusto_cluster.this[0].uri : ""
    portal_client_id             = var.enable_adx ? azurerm_user_assigned_identity.portal[0].client_id : ""
    cluster_name                 = var.cluster_name
  })
}

resource "azurerm_kusto_cluster" "this" {
  count               = var.enable_adx ? 1 : 0
  name                = var.adx_cluster_name
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  auto_stop_enabled   = true

  sku {
    name     = var.adx_sku_name
    capacity = var.adx_sku_capacity
  }

  tags = var.tags
}

resource "azurerm_kusto_database" "this" {
  for_each            = var.enable_adx ? local.adx_databases : toset([])
  name                = each.value
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  cluster_name        = azurerm_kusto_cluster.this[0].name
  soft_delete_period  = "P7D"
  hot_cache_period    = "P1D"
}

resource "azurerm_user_assigned_identity" "portal" {
  count               = var.enable_adx ? 1 : 0
  name                = "taugrid-portal-adx"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
}

resource "azurerm_user_assigned_identity" "adx_mon" {
  count               = var.enable_adx ? 1 : 0
  name                = "taugrid-adx-mon"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
}

resource "azurerm_user_assigned_identity" "lifecycle_recorder" {
  count               = var.enable_lifecycle_recorder ? 1 : 0
  name                = "taugrid-lifecycle-recorder"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
}

resource "azurerm_federated_identity_credential" "portal" {
  count               = var.enable_adx ? 1 : 0
  name                = "tau-portal"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.portal[0].id
  issuer              = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject             = "system:serviceaccount:tau:tau-portal"
  audience            = ["api://AzureADTokenExchange"]
}

resource "azurerm_federated_identity_credential" "adx_mon_ingestor" {
  count               = var.enable_adx ? 1 : 0
  name                = "adx-mon-ingestor"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.adx_mon[0].id
  issuer              = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject             = "system:serviceaccount:adx-mon:adx-mon-ingestor"
  audience            = ["api://AzureADTokenExchange"]
}

resource "azurerm_federated_identity_credential" "adx_mon_alerter" {
  count               = var.enable_adx ? 1 : 0
  name                = "adx-mon-alerter"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.adx_mon[0].id
  issuer              = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject             = "system:serviceaccount:adx-mon:adx-mon-alerter"
  audience            = ["api://AzureADTokenExchange"]
}

resource "azurerm_federated_identity_credential" "lifecycle_recorder" {
  count               = var.enable_lifecycle_recorder ? 1 : 0
  name                = "tau-lifecycle-recorder"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.lifecycle_recorder[0].id
  issuer              = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject             = "system:serviceaccount:tau:tau-lifecycle-recorder"
  audience            = ["api://AzureADTokenExchange"]
}

resource "azurerm_kusto_database_principal_assignment" "adx_mon" {
  for_each            = var.enable_adx ? local.adx_databases : toset([])
  name                = "taugrid-adx-mon-${lower(each.value)}"
  resource_group_name = azurerm_resource_group.this.name
  cluster_name        = azurerm_kusto_cluster.this[0].name
  database_name       = azurerm_kusto_database.this[each.value].name
  principal_id        = azurerm_user_assigned_identity.adx_mon[0].client_id
  principal_type      = "App"
  role                = "Admin"
  tenant_id           = azurerm_user_assigned_identity.adx_mon[0].tenant_id
}

resource "azurerm_kusto_database_principal_assignment" "portal" {
  count               = var.enable_adx ? 1 : 0
  name                = "taugrid-portal-viewer"
  resource_group_name = azurerm_resource_group.this.name
  cluster_name        = azurerm_kusto_cluster.this[0].name
  database_name       = azurerm_kusto_database.this["Metrics"].name
  principal_id        = azurerm_user_assigned_identity.portal[0].client_id
  principal_type      = "App"
  role                = "Viewer"
  tenant_id           = azurerm_user_assigned_identity.portal[0].tenant_id
}

resource "azurerm_kusto_database_principal_assignment" "lifecycle_recorder" {
  count               = var.enable_lifecycle_recorder ? 1 : 0
  name                = "taugrid-lifecycle-recorder-ingestor"
  resource_group_name = azurerm_resource_group.this.name
  cluster_name        = azurerm_kusto_cluster.this[0].name
  database_name       = azurerm_kusto_database.this["Metrics"].name
  principal_id        = azurerm_user_assigned_identity.lifecycle_recorder[0].client_id
  principal_type      = "App"
  role                = "Ingestor"
  tenant_id           = azurerm_user_assigned_identity.lifecycle_recorder[0].tenant_id
}

resource "local_file" "taugrid_values" {
  filename        = local.values_path
  content         = local.taugrid_values
  file_permission = "0600"
}

resource "local_file" "mig_normalizer" {
  count           = var.normalize_gpu_mig ? 1 : 0
  filename        = "${local.generated_directory}/mig-normalizer.yaml"
  file_permission = "0600"
  content = templatefile("${path.module}/mig-normalizer.yaml.tftpl", {
    gpu_node_pool_name = var.gpu_node_pool_name
  })
}

resource "terraform_data" "install_nvidia_device_plugin" {
  triggers_replace = [
    azurerm_kubernetes_cluster_node_pool.gpu.id,
    filesha256("${path.module}/nvidia-device-plugin.yaml"),
    join(" ", var.command_interpreter),
  ]

  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --admin --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --file '${local.kubeconfig_path}' --overwrite-existing && kubectl apply -f nvidia-device-plugin.yaml && kubectl rollout status daemonset/nvidia-device-plugin-daemonset --namespace kube-system --timeout=10m"
  }

  depends_on = [
    azurerm_kubernetes_cluster_node_pool.gpu,
  ]
}

resource "terraform_data" "normalize_gpu_mig" {
  count = var.normalize_gpu_mig ? 1 : 0

  triggers_replace = [
    azurerm_kubernetes_cluster_node_pool.gpu.id,
    local_file.mig_normalizer[0].content_sha256,
    var.gpu_count_per_node,
    join(" ", var.command_interpreter),
  ]

  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --admin --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --file '${local.kubeconfig_path}' --overwrite-existing && kubectl apply -f '${local_file.mig_normalizer[0].filename}' && kubectl rollout status daemonset/taugrid-mig-normalizer --namespace kube-system --timeout=10m && az vmss restart --subscription '${var.subscription_id}' --resource-group $(az aks show --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --query nodeResourceGroup --output tsv) --name $(az vmss list --subscription '${var.subscription_id}' --resource-group $(az aks show --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --query nodeResourceGroup --output tsv) --query \"[?starts_with(name, 'aks-${var.gpu_node_pool_name}-')].name | [0]\" --output tsv) && kubectl wait --for=condition=Ready node --selector agentpool='${var.gpu_node_pool_name}' --timeout=20m && kubectl wait --for='jsonpath={.status.allocatable.nvidia\\.com/gpu}'='${var.gpu_count_per_node}' node --selector agentpool='${var.gpu_node_pool_name}' --timeout=20m && kubectl delete -f '${local_file.mig_normalizer[0].filename}' --ignore-not-found"
  }

  depends_on = [
    azurerm_kubernetes_cluster_node_pool.gpu,
    local_file.mig_normalizer,
    terraform_data.install_nvidia_device_plugin,
  ]
}

resource "terraform_data" "install_taugrid" {
  count = var.install_taugrid ? 1 : 0

  triggers_replace = [
    azurerm_kubernetes_cluster_node_pool.gpu.id,
    local_file.taugrid_values.content_sha256,
    var.taugrid_version,
    join(" ", var.command_interpreter),
  ]

  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --admin --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --file '${local.kubeconfig_path}' --overwrite-existing && tau cluster install --values '${local_file.taugrid_values.filename}' --version '${var.taugrid_version}' --timeout 20m"
  }

  depends_on = [
    local_file.taugrid_values,
    azurerm_kubernetes_cluster_node_pool.gpu,
    terraform_data.install_nvidia_device_plugin,
    terraform_data.normalize_gpu_mig,
    azurerm_federated_identity_credential.lifecycle_recorder,
    azurerm_kusto_database_principal_assignment.lifecycle_recorder,
    terraform_data.install_adx_mon,
  ]
}

resource "local_file" "adx_mon_values" {
  count           = var.enable_adx ? 1 : 0
  filename        = "${local.generated_directory}/adx-mon-values.yaml"
  file_permission = "0600"
  content = templatefile("${path.module}/adx-mon-values.yaml.tftpl", {
    adx_endpoint       = azurerm_kusto_cluster.this[0].uri
    adx_client_id      = azurerm_user_assigned_identity.adx_mon[0].client_id
    cluster_name       = var.cluster_name
    location           = var.location
    gpu_node_pool_name = var.gpu_node_pool_name
  })
}

resource "terraform_data" "remove_legacy_dcgm_exporter" {
  count = var.enable_adx ? 1 : 0

  triggers_replace = [
    azurerm_kubernetes_cluster.this.id,
    join(" ", var.command_interpreter),
  ]

  # Earlier Terraform revisions installed this release. terraform_data does
  # not remove external Helm state when its configuration is deleted, so clean
  # it up explicitly before switching adx-mon to the AKS host DCGM endpoint.
  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --admin --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --file '${local.kubeconfig_path}' --overwrite-existing && helm uninstall dcgm-exporter --namespace dcgm-exporter --ignore-not-found"
  }
}

resource "terraform_data" "install_adx_mon" {
  count = var.enable_adx ? 1 : 0

  triggers_replace = [
    azurerm_kubernetes_cluster.this.id,
    azurerm_kubernetes_cluster_node_pool.gpu.id,
    local_file.adx_mon_values[0].content_sha256,
    azurerm_kusto_database_principal_assignment.adx_mon,
    join(" ", var.command_interpreter),
  ]

  provisioner "local-exec" {
    working_dir = path.module
    interpreter = var.command_interpreter
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
    command = "az aks get-credentials --admin --subscription '${var.subscription_id}' --resource-group '${azurerm_resource_group.this.name}' --name '${azurerm_kubernetes_cluster.this.name}' --file '${local.kubeconfig_path}' --overwrite-existing && helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update && helm repo update prometheus-community && helm dependency build ../../charts/adx-mon && helm upgrade --install adx-mon ../../charts/adx-mon --namespace adx-mon --create-namespace --values ../../charts/adx-mon/values-ai-runtime.yaml --values '${local_file.adx_mon_values[0].filename}' --wait --timeout 30m"
  }

  depends_on = [
    terraform_data.remove_legacy_dcgm_exporter,
    azurerm_kusto_database_principal_assignment.adx_mon,
    azurerm_kusto_database_principal_assignment.portal,
    azurerm_kusto_database_principal_assignment.lifecycle_recorder,
  ]
}
