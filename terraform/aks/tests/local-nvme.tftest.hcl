# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

mock_provider "azurerm" {}
mock_provider "azapi" {}
mock_provider "local" {}

variables {
  subscription_id     = "00000000-0000-0000-0000-000000000001"
  tenant_id           = "00000000-0000-0000-0000-000000000002"
  resource_group_name = "taugrid-test"
  cluster_name        = "taugrid-test"
  generated_directory = "generated/plan-test"
}

run "disabled_by_default" {
  command = plan

  assert {
    condition = (
      length(local_file.local_csi_driver_values) == 0 &&
      length(terraform_data.require_no_local_csi_driver) == 0 &&
      length(terraform_data.install_local_csi_driver) == 0
    )
    error_message = "local_nvme_enabled must default to false and plan no local NVMe resources."
  }
}

run "enabled" {
  command = plan

  variables {
    local_nvme_enabled = true
    gpu_node_pool_name = "nvme"
  }

  assert {
    condition = (
      length(local_file.local_csi_driver_values) == 1 &&
      length(terraform_data.require_no_local_csi_driver) == 1 &&
      length(terraform_data.install_local_csi_driver) == 1
    )
    error_message = "local_nvme_enabled must plan the values file, the existing-driver check, and the install."
  }

  assert {
    condition = (
      yamldecode(local_file.local_csi_driver_values[0].content).daemonset.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms == [
        {
          matchExpressions = [
            {
              key      = "kubernetes.azure.com/agentpool"
              operator = "In"
              values   = ["nvme"]
            },
            {
              key      = "kubernetes.azure.com/nodepool-type"
              operator = "NotIn"
              values   = ["FlexNodes"]
            },
          ]
        },
      ]
    )
    error_message = "The driver must run only on the GPU pool and never on Flex nodes."
  }

  assert {
    condition     = contains(terraform_data.install_local_csi_driver[0].triggers_replace, "0.4.1")
    error_message = "Changing the pinned chart version must reinstall the driver."
  }
}
