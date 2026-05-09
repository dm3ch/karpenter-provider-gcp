# GPU Nodes

Karpenter can provision GPU-equipped GKE nodes for workloads that request `nvidia.com/gpu` resources.

## Instance types

**Built-in GPU families** — `a2`, `a3`, `g2`, `a4`. These machine families have NVIDIA accelerators integrated into the machine type. Select them via `karpenter.k8s.gcp/instance-gpu-count`.

> **Note:** Attached GPU instances (e.g. `n1-standard-*` + NVIDIA T4/P100/V100 accelerators) are not yet fully supported. Tracked in [#84](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/84).

## Device plugin scheduling

Karpenter automatically injects the `cloud.google.com/gke-accelerator=<type>` label into the node's `kube-labels` metadata for all GPU instances. This label is required by the NVIDIA device plugin DaemonSet's `nodeAffinity`, so without it the plugin would not schedule onto Karpenter-provisioned GPU nodes and `nvidia.com/gpu` would never become allocatable.

No additional configuration is needed — the label is derived from the instance type's accelerator type and injected at provisioning time.

## Driver installation

Karpenter automatically installs the GKE-recommended stable NVIDIA driver on GPU nodes. Set `gpuDriverVersion` to choose the driver version or opt out:

```yaml
spec:
  gpuDriverVersion: latest   # or "default" (default) / "disabled"
```

| Value      | Terraform equivalent    | Behaviour                                                             |
|------------|-------------------------|-----------------------------------------------------------------------|
| `default`  | `DEFAULT`               | GKE-recommended stable driver. Works on COS and Ubuntu. (**default**) |
| `latest`   | `LATEST`                | Newest available driver. COS only.                                    |
| `disabled` | `INSTALLATION_DISABLED` | Skip automatic driver installation. Manage drivers manually.          |

Karpenter injects `cloud.google.com/gke-gpu-driver-version=<value>` as a node label at provisioning time. GKE's GPU driver installer DaemonSet reads this label to determine which driver to install. The label is only set on GPU instances — non-GPU instances are unaffected.

## Example

See [`docs/examples/gpu.md`](examples/gpu.md) for a complete `GCENodeClass` + `NodePool` configuration.
