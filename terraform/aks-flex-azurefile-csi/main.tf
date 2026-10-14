provider "helm" {
  kubernetes = {
    config_path    = pathexpand(var.kubeconfig_path)
    config_context = var.kube_context
  }
}

provider "kubernetes" {
  config_path    = pathexpand(var.kubeconfig_path)
  config_context = var.kube_context
}

locals {
  controller_name = "csi-azurefile-flex-controller"
  linux_ds_name   = "csi-azurefile-flex-node"
  rbac_name       = "azurefile-flex"
  linux_tolerations = [
    for toleration in var.linux_tolerations : {
      for key, value in toleration : key => value if value != null
    }
  ]
}

resource "helm_release" "azurefile_csi" {
  name       = var.release_name
  namespace  = var.namespace
  repository = "https://kubernetes-sigs.github.io/azurefile-csi-driver"
  chart      = "azurefile-csi-driver"
  version    = var.azurefile_csi_version

  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  wait             = true
  timeout          = 1200

  values = [
    yamlencode({
      driver = {
        name            = var.driver_name
        userAgentSuffix = "taugrid-flex-terraform"
      }
      controller = {
        name        = local.controller_name
        hostNetwork = false
        metricsPort = 39614
        livenessProbe = {
          healthPort = 39612
        }
        nodeSelector = {
          "kubernetes.io/os" = "linux"
        }
      }
      node = {
        enableManagedIdentityAuth = false
        metricsPort               = 39615
        livenessProbe = {
          healthPort = 39613
        }
        nodeDriverRegistrar = {
          healthPort = 39617
        }
        azurefileProxy = {
          enabled           = false
          installAznfsMount = false
        }
      }
      rbac = {
        name = local.rbac_name
      }
      serviceAccount = {
        controller = "csi-azurefile-flex-controller-sa"
        node       = "csi-azurefile-flex-node-sa"
      }
      linux = {
        dsName       = local.linux_ds_name
        nodeSelector = merge(var.flex_node_selector, { "kubernetes.io/os" = "linux" })
        tolerations  = local.linux_tolerations
      }
      windows = {
        enabled = false
      }
      snapshot = {
        enabled = false
      }
    })
  ]
}

resource "kubernetes_storage_class_v1" "azurefile_flex" {
  count = var.create_storage_class ? 1 : 0

  metadata {
    name = var.storage_class_name
  }

  storage_provisioner    = var.driver_name
  reclaim_policy         = var.reclaim_policy
  volume_binding_mode    = "Immediate"
  allow_volume_expansion = true

  parameters = {
    skuName                                                 = var.storage_sku_name
    protocol                                                = "smb"
    storageAccount                                          = var.storage_account_name
    resourceGroup                                           = var.storage_account_resource_group
    subscriptionID                                          = var.storage_account_subscription_id
    "csi.storage.k8s.io/provisioner-secret-name"            = var.storage_secret_name
    "csi.storage.k8s.io/provisioner-secret-namespace"       = "$${pvc.namespace}"
    "csi.storage.k8s.io/node-stage-secret-name"             = var.storage_secret_name
    "csi.storage.k8s.io/node-stage-secret-namespace"        = "$${pvc.namespace}"
    "csi.storage.k8s.io/controller-expand-secret-name"      = var.storage_secret_name
    "csi.storage.k8s.io/controller-expand-secret-namespace" = "$${pvc.namespace}"
  }

  mount_options = [
    "dir_mode=0770",
    "file_mode=0660",
    "mfsymlinks",
    "cache=strict",
    "nosharesock",
    "actimeo=30",
  ]

  lifecycle {
    precondition {
      condition = !var.create_storage_class || alltrue([
        length(trimspace(var.storage_account_name)) > 0,
        length(trimspace(var.storage_account_resource_group)) > 0,
        length(trimspace(var.storage_account_subscription_id)) > 0,
        length(trimspace(var.storage_secret_name)) > 0,
      ])
      error_message = "Creating the StorageClass requires the existing storage account, resource group, subscription ID, and pre-created Secret name."
    }
  }

  depends_on = [helm_release.azurefile_csi]
}
