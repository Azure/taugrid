mock_provider "helm" {}
mock_provider "kubernetes" {}

variables {
  kube_context = "test-flex"
  flex_node_selector = {
    "kubernetes.azure.com/cluster" = "flex-test"
  }
  ownership_reviewed = true
}

run "installs_distinct_flex_driver" {
  command = plan

  assert {
    condition     = helm_release.azurefile_csi.name == "azurefile-csi-flex"
    error_message = "The Flex driver must use its own Helm release."
  }

  assert {
    condition     = yamldecode(helm_release.azurefile_csi.values[0]).driver.name == "file.flex.csi.azure.com"
    error_message = "The Flex driver must not reuse the AKS-managed CSI identity."
  }

  assert {
    condition     = yamldecode(helm_release.azurefile_csi.values[0]).linux.nodeSelector["kubernetes.azure.com/cluster"] == "flex-test"
    error_message = "The node plugin must retain the caller's Flex node selector."
  }

  assert {
    condition     = yamldecode(helm_release.azurefile_csi.values[0]).windows.enabled == false
    error_message = "The Flex driver root supports Linux nodes only."
  }

  assert {
    condition = alltrue([
      yamldecode(helm_release.azurefile_csi.values[0]).node.azurefileProxy.enabled == false,
      yamldecode(helm_release.azurefile_csi.values[0]).node.azurefileProxy.installAznfsMount == false,
      yamldecode(helm_release.azurefile_csi.values[0]).node.enableManagedIdentityAuth == false,
    ])
    error_message = "Secret-backed SMB must not mutate node-global proxy/AZNFS services or start managed-identity helpers."
  }
}

run "creates_matching_storage_class" {
  command = plan

  variables {
    create_storage_class            = true
    storage_account_name            = "teststorage"
    storage_account_resource_group  = "test-storage-rg"
    storage_account_subscription_id = "00000000-0000-0000-0000-000000000000"
  }

  assert {
    condition     = kubernetes_storage_class_v1.azurefile_flex[0].storage_provisioner == "file.flex.csi.azure.com"
    error_message = "The Flex StorageClass must use the customer-managed driver."
  }

  assert {
    condition     = kubernetes_storage_class_v1.azurefile_flex[0].volume_binding_mode == "Immediate"
    error_message = "TauGrid requires platform-managed PVCs to bind before workload submission."
  }

  assert {
    condition = alltrue([
      kubernetes_storage_class_v1.azurefile_flex[0].parameters["csi.storage.k8s.io/provisioner-secret-namespace"] == "$${pvc.namespace}",
      kubernetes_storage_class_v1.azurefile_flex[0].parameters["csi.storage.k8s.io/node-stage-secret-namespace"] == "$${pvc.namespace}",
      kubernetes_storage_class_v1.azurefile_flex[0].parameters["csi.storage.k8s.io/controller-expand-secret-namespace"] == "$${pvc.namespace}",
    ])
    error_message = "The external provisioner must resolve credentials from each PVC namespace."
  }
}
