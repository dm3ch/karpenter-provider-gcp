# Karpenter GCP review guidance

When reviewing provider changes, check that package boundaries stay clear:

- `pkg/providers/instance` owns GCE VM lifecycle, instance labels/tags, scheduling, capacity errors, and zone operations.
- `pkg/providers/nodepooltemplate` owns GKE bootstrap source pool selection, fallback pool creation, and source instance-template discovery.
- `pkg/providers/gke` owns shared GKE cluster facts such as cluster config, server config, cluster identity, and zone resolution.
- `pkg/metadata` owns pure typed GKE bootstrap metadata modeling, parsing, typed mutation helpers, and Compute API rendering only.

For `pkg/metadata`, flag any change that adds GCP/Kubernetes API calls, source node-pool or instance-template discovery, GCE VM lifecycle decisions, scheduler/capacity policy, or provider-owned bootstrap decisions. Provider policy such as labels, taints, provisioning model, GPU behavior, disk compatibility, secondary boot disks, kubelet defaults, Spot shutdown behavior, and NodeClass overlays belongs in provider packages and should be passed into `pkg/metadata` through typed helpers.

Flag changes that duplicate GKE API reads across providers, let bootstrap metadata helpers perform API calls, mutate caller-owned Compute metadata templates in place, or mix Kubernetes object metadata with GCE instance labels/tags.

## GCENodeClass ergonomics: follow GKE node configuration

For new or materially changed GCENodeClass settings with a GKE equivalent, prefer the names, nesting, and semantics operators already know from [GKE NodeConfig](https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/NodeConfig) and [Terraform `google_container_node_pool.node_config`](https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/container_node_pool#node_config). Translate Terraform snake_case to Kubernetes camelCase; do not copy HCL syntax or add a `spec.nodeConfig` wrapper, since `spec` already holds node configuration.

Examples motivated by review of [#718](https://github.com/karpenter-gcp/karpenter-provider-gcp/pull/718#discussion_r4432047492) and [#727](https://github.com/karpenter-gcp/karpenter-provider-gcp/pull/727):

- Prefer `spec.advancedMachineFeatures.threadsPerCore` (Terraform `node_config.advanced_machine_features.threads_per_core`), not a flat `spec.threadsPerCore`.
- Prefer `spec.gvnic.enabled` (Terraform `node_config.gvnic.enabled`), not a raw GCE `spec.networkConfig.nicType` enum.
- Keep `spec.minCpuPlatform` aligned with GKE `minCpuPlatform` / Terraform `node_config.min_cpu_platform`.

Compare types, allowed values, units, defaults, and omitted versus explicit false/zero behavior, not just spelling. Allow Kubernetes API conventions and documented differences required by Karpenter scheduling, direct GCE provisioning, or backward compatibility. Do not demand Terraform resource lifecycle behavior, support for every GKE feature, or renames of unchanged existing fields.

Only flag concrete, avoidable mismatches introduced or materially changed by the PR. Cite the equivalent GKE/Terraform field and reference, suggest the aligned API shape, and account for any documented reason for divergence. Verify that generated CRDs, reference docs, and examples match the chosen API; do not report an already-correct nested field or a justified exception as a violation.

## Fuzz coverage for untrusted-input parsers and verifiers

For new or materially changed Go code that decodes or verifies bytes an untrusted party controls, expect a native Go fuzz target. In scope: the CSR `request` field and anything inside it (PEM, DER/ASN.1, JSON, TPM structures), signatures and proofs, and other data supplied by a requester, kubelet or VM. Out of scope: values already decoded by Kubernetes or GCP client libraries, pure policy or state logic that table tests cover better, thin wrappers around a standard-library parser with no logic of their own, generated code and `vendor/`.

A good target is a `FuzzXxx(f *testing.F)` function in a `fuzz_test.go` or `*_fuzz_test.go` file next to the code. It needs seeds for a valid input and its empty, truncated and corrupted forms, and it asserts a stated property: arbitrary input never panics, invalid input is never accepted, and accepted input satisfies the invariant the caller relies on. Targets must be deterministic and offline, because `go test` runs every seed as an ordinary unit test and `make fuzz` (part of `make ci`) explores each target for a bounded time. Use `pkg/controllers/csr/fuzz_test.go` as the pattern.

Recommend a target only when no equivalent exists and the code has its own parsing or verification logic. Do not ask for longer fuzzing campaigns, coverage targets, or fuzzing of refactors that leave parsing unchanged. Treat a missing target as a medium-severity gap, and a target with no stated property or no seeds as a low-severity one. When fuzzing finds a crasher, expect the minimized input committed under `testdata/fuzz/` as a regression case.

## End-to-end coverage opportunities

For new or materially changed user-visible behavior, consider whether an end-to-end test would cover integration risk that unit tests cannot. Recommend one only when the behavior crosses real GKE, GCP, Kubernetes, controller, or node boundaries; has stable observable outcomes; and can be exercised economically in the existing `test/suites` environment.

Do not request end-to-end tests for refactors, generated or documentation-only changes, or behavior already well covered below cloud and process boundaries. When a test is worthwhile, explain the uncovered risk and propose a compact outline:

- existing or new suite and spec;
- minimum setup;
- action under test;
- key user-visible assertions;
- cleanup, preferably through existing suite helpers.
