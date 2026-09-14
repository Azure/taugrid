# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Plan only: no provider calls, provisioners, resources, or deployment state.
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
    condition     = length(azapi_resource.lifecycle_recorder_principal_assignment) == 0 && length(azurerm_user_assigned_identity.lifecycle_recorder) == 0
    error_message = "Default-disabled lifecycle recording must not create an identity or grant."
  }
}

run "adx_without_recorder" {
  command = plan

  variables {
    enable_adx = true
  }

  assert {
    condition     = length(azapi_resource.lifecycle_recorder_principal_assignment) == 0 && length(azurerm_user_assigned_identity.lifecycle_recorder) == 0
    error_message = "ADX alone must not enable lifecycle recording."
  }
}

run "fresh_recorder" {
  command = plan

  variables {
    enable_adx                = true
    enable_lifecycle_recorder = true
  }

  assert {
    condition     = length(azapi_resource.lifecycle_recorder_principal_assignment) == 1
    error_message = "Enabled lifecycle recording must plan exactly one grant."
  }

  assert {
    condition = (
      azapi_resource.lifecycle_recorder_principal_assignment[0].name == "taugrid-lifecycle-recorder-ingestor" &&
      azapi_resource.lifecycle_recorder_principal_assignment[0].body.properties.principalType == "App" &&
      azapi_resource.lifecycle_recorder_principal_assignment[0].body.properties.role == "Ingestor"
    )
    error_message = "The recorder must retain its named App/Ingestor grant."
  }

  assert {
    condition = (
      azapi_resource.lifecycle_recorder_principal_assignment[0].retry.error_message_regex == tolist(["(?i)AAD principal was not found"]) &&
      azapi_resource.lifecycle_recorder_principal_assignment[0].retry.interval_seconds == 10 &&
      azapi_resource.lifecycle_recorder_principal_assignment[0].retry.max_interval_seconds == 180 &&
      azapi_resource.lifecycle_recorder_principal_assignment[0].timeouts.create == "60m"
    )
    error_message = "Principal propagation retries must retain their narrow classifier and bounds."
  }
}

run "recorder_requires_adx" {
  command = plan

  variables {
    enable_lifecycle_recorder = true
  }

  expect_failures = [var.enable_lifecycle_recorder]
}
