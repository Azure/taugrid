provider "helm" {
  kubernetes = {
    config_path    = pathexpand(var.kubeconfig_path)
    config_context = var.kube_context
  }
}

resource "helm_release" "gpu_operator" {
  name       = "gpu-operator"
  namespace  = var.namespace
  repository = "https://helm.ngc.nvidia.com/nvidia"
  chart      = "gpu-operator"
  version    = var.gpu_operator_version

  create_namespace = true
  atomic           = true
  cleanup_on_fail  = true
  wait             = true
  timeout          = 1800

  values = [
    yamlencode({
      driver = {
        # AKS Flex images provide the host driver. A second driver owner can
        # leave nodes unbootable or make the device plugin report no GPUs.
        enabled = false
      }
      toolkit = {
        enabled = var.toolkit_enabled
        env = [
          {
            name  = "ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS"
            value = "true"
          }
        ]
      }
      devicePlugin = {
        enabled = true
      }
      dcgmExporter = {
        enabled = true
        service = {
          internalTrafficPolicy = "Local"
        }
        serviceMonitor = {
          enabled = false
        }
      }
      mig = {
        strategy = var.mig_strategy
      }
      migManager = {
        enabled = var.mig_manager_enabled
        config = {
          default = "all-disabled"
        }
        env = [
          {
            name  = "WITH_REBOOT"
            value = "false"
          }
        ]
      }
      nodeStatusExporter = {
        enabled = false
      }
      daemonsets = {
        tolerations = [
          {
            key      = "sku"
            operator = "Equal"
            value    = "gpu"
            effect   = "NoSchedule"
          },
          {
            key      = "nvidia.com/gpu"
            operator = "Exists"
            effect   = "NoSchedule"
          }
        ]
      }
      "node-feature-discovery" = {
        worker = {
          tolerations = [
            {
              key      = "node-role.kubernetes.io/control-plane"
              operator = "Equal"
              effect   = "NoSchedule"
            },
            {
              key      = "nvidia.com/gpu"
              operator = "Exists"
              effect   = "NoSchedule"
            },
            {
              key      = "sku"
              operator = "Equal"
              value    = "gpu"
              effect   = "NoSchedule"
            }
          ]
        }
      }
    })
  ]
}
