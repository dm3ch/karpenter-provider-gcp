# Running alongside GKE autoscaling

Karpenter can share a GKE Standard cluster with the GKE cluster autoscaler, node auto-provisioning (NAP), and custom ComputeClasses. This setup isn't recommended. Depending on your configuration, both provisioners can act on the same pods, and the result is [undefined](#how-the-provisioners-overlap). Run both only when one provisioner can't cover your workloads. For example, you might keep GKE-managed node pools for accelerators Karpenter doesn't support yet. This page explains how to give each workload to exactly one provisioner.

## How the provisioners overlap

Each provisioner removes only the nodes it created. Karpenter consolidates and deletes only the nodes it launched. Karpenter nodes are standalone GCE VMs, not members of a GKE node pool, so the cluster autoscaler doesn't resize or remove them.

Both provisioners react to the same pending pods. When a pending pod fits a Karpenter NodePool and also fits a GKE node pool or ComputeClass, both can add a node for it. Which one runs the pod is undefined, and the unused node stays until its provisioner's scale-down removes it. To avoid this, make sure every pending pod matches only one provisioner.

Each provisioner skips pods whose scheduling constraints it can't satisfy:

| A pending pod that…                                                                                             | Karpenter                                                                                         | Cluster autoscaler and NAP                           |
|-----------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------|------------------------------------------------------|
| Selects `cloud.google.com/compute-class`, `cloud.google.com/gke-nodepool`, or `cloud.google.com/machine-family` | Skips it, unless a NodePool defines that label                                                    | Can act on it                                        |
| Selects `karpenter.sh/nodepool`                                                                                 | Can act on it                                                                                     | Skip it, because no GKE node pool carries that label |
| Selects `cloud.google.com/gke-accelerator`                                                                      | Can act on it if a NodePool allows GPU instance types and the pod tolerates the NodePool's taints | Can act on it                                        |
| Doesn't tolerate the taints on any Karpenter NodePool                                                           | Skips it                                                                                          | Can act on it                                        |
| Has no node selectors and tolerates the taints on a Karpenter NodePool, or the NodePool has none                | Can act on it                                                                                     | Can act on it                                        |

A toleration alone doesn't keep a pod off untainted nodes. The cluster autoscaler can still scale up an untainted node pool for a pod that tolerates the Karpenter taint. Pair the toleration with a node selector.

## Send workloads to Karpenter

Use this setup when GKE node pools, NAP, or ComputeClasses handle most workloads and only some workloads should run on Karpenter nodes.

1. Add a taint to every Karpenter NodePool. Karpenter then skips pods that don't tolerate it. This partial NodePool shows only the taint; keep your existing requirements and disruption settings:

   ```yaml
   apiVersion: karpenter.sh/v1
   kind: NodePool
   metadata:
     name: general
   spec:
     template:
       spec:
         nodeClassRef:
           group: karpenter.k8s.gcp
           kind: GCENodeClass
           name: default
         taints:
           - key: provisioner
             value: karpenter
             effect: NoSchedule
   ```

2. In each workload that should run on Karpenter nodes, select the NodePool and tolerate its taint:

   ```yaml
   spec:
     nodeSelector:
       karpenter.sh/nodepool: general
     tolerations:
       - key: provisioner
         operator: Equal
         value: karpenter
         effect: NoSchedule
   ```

Keep the toleration key different from the node selector key. NAP can create a node pool for a pending pod that selects a custom label and tolerates a taint with the same key. The new pool carries that label and taint. Karpenter would then compete for the pod. For example, don't route with a `team: batch` selector and a `team=batch:NoSchedule` taint.

If a namespace has the `cloud.google.com/default-compute-class` or `cloud.google.com/default-compute-class-non-daemonset` label, GKE adds a ComputeClass selector to new pods in it. Karpenter skips those pods, so run Karpenter workloads in namespaces without these labels.

## Send workloads to GKE

Use this setup when Karpenter handles most workloads and only some workloads should run on GKE-managed nodes.

- Disable cluster-level NAP. It acts on any pending pod that no existing node pool fits, including pods meant for Karpenter. On GKE `1.33.3-gke.1136000` and later, set `nodePoolAutoCreation.enabled: true` in a ComputeClass instead, so GKE creates node pools only for pods that select that ComputeClass.
- Taint every node pool that has the cluster autoscaler enabled, so it scales up only for pods that tolerate the taint. Leave a node pool untainted only if it has a fixed size, such as a system pool.
- In each workload that should run on GKE nodes, select the ComputeClass with `cloud.google.com/compute-class` or the node pool with `cloud.google.com/gke-nodepool`, and tolerate the node pool's taint. Karpenter skips pods with these selectors.

## Pin the bootstrap pool

Karpenter reads bootstrap metadata from a GKE node pool. Automatic discovery can select a node pool that NAP or a ComputeClass created, and GKE deletes such a pool when it has no nodes. Pin Karpenter to a manually created pool with `controller.settings.defaultNodePoolTemplateName`. See [Pinning a specific pool](bootstrap-pool.md#pinning-a-specific-pool).
