# Provider performance checks

The CodSpeed workflow exercises provider workloads without a Kubernetes cluster, cloud credentials, or live GCP calls. It complements the unit tests; it does not measure the resource consumption of a deployed controller.

## Workloads

| Benchmark                                                   | Purpose                                                                                                                                                                                                                |
|-------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `BenchmarkInstanceTypeProviderList`                         | Real catalog expansion and offering injection with 3 or 15 distinct E2, N2 and T2A machine shapes. N2 local-SSD variants expand the returned catalog further. Warm calls reuse static shapes; cold calls rebuild them. |
| `BenchmarkGetServerConfig`                                  | Real GKE provider cache lookups using an unauthenticated local HTTP server. Warm calls must make zero additional requests; refresh calls must make one request per lookup.                                             |
| `BenchmarkFromSourceTemplate`, `BenchmarkToComputeMetadata` | Secondary coverage of bootstrap metadata parsing and rendering.                                                                                                                                                        |

One catalog operation is a complete `List`, not one machine. `variants/op` records its returned size. Cold and refresh cases include cache reset in the measured operation so native Go and CodSpeed do not depend on different timer exclusions. Fixture construction and the first cache fill happen before measurement. Fixture caches have no background cleanup goroutines.

The standard GKE tests protect cache reuse and refreshing expired configuration. The benchmark also checks its request budget. These are deterministic failures, not timing thresholds. Requests are counted at the local HTTP boundary, not at a mocked cache method. No singleflight/concurrent-miss behavior is promised.

## Metrics and PR feedback

| Signal                          | Where to read it                        | What it establishes                                                                                           |
|---------------------------------|-----------------------------------------|---------------------------------------------------------------------------------------------------------------|
| Timing                          | CodSpeed PR report/check                | Wall time per workload, compared with an available baseline. Shared GitHub runners can be noisy.              |
| `B/op`, `allocs/op`             | Native metrics job summary and artifact | Allocation traffic per operation, not retained heap, process RSS, or a memory leak test.                      |
| `api-requests/op`               | Native GKE benchmark output             | Backend request amplification: zero on warm reads, one on forced refresh. This is not a dollar-cost estimate. |
| Cache freshness/request budgets | Unit tests and benchmark assertions     | Correct reuse and refresh behavior; a violation fails CI even without a CodSpeed baseline.                    |

Native metrics are informational, not a second before/after regression gate. CodSpeed's Go integration currently supports walltime, not its CPU-simulation or memory instruments. Its compatibility harness can print zero allocation fields for allocating workloads; those are not memory measurements. Read allocation metrics from the native step instead. Do not interpret latency as controller CPU usage. A local HTTP refresh includes the test server and SDK work, not actual GCP latency.

A first successful run on a new branch proves that benchmarks executed, not that performance did not regress. New benchmarks need a comparable main-branch run before CodSpeed can report meaningful deltas. Changing fixtures, Go versions or runner settings can also change the baseline. Do not enable a tight required walltime threshold without repeated baseline and regression-sensitivity checks.

## Run locally

Run the native metrics and relevant API-budget tests:

```sh
go test -run='^TestGetServerConfig_' \
  -bench='Benchmark(InstanceTypeProviderList|GetServerConfig|FromSourceTemplate|ToComputeMetadata)$' \
  -benchmem -benchtime=200ms -count=3 \
  ./pkg/providers/instancetype ./pkg/providers/gke ./pkg/metadata
```

For CodSpeed, use the same benchmark selection without native-only flags:

```sh
codspeed run --mode walltime --skip-upload -- \
  go test -bench='Benchmark(InstanceTypeProviderList|GetServerConfig|FromSourceTemplate|ToComputeMetadata)$' \
  -benchtime=1s ./pkg/providers/instancetype ./pkg/providers/gke ./pkg/metadata
```

The workflow pins the CodSpeed runner and Go harness. The pinned Go harness supports `-bench` and `-benchtime`; it ignores `-run`, so the CodSpeed invocation omits it. `ReportAllocs` and custom request counters are retained in the native output; their presence does not imply the CodSpeed dashboard compares them.

## Why not add Bencher too?

CodSpeed provides timing reports and optional Wizard explanations/fix proposals in the existing PR workflow. Bencher is a continuous-benchmark result store with statistical thresholds and custom measures; its standard Go adapter also imports latency only. Sending allocation/request metrics to Bencher would require a custom-format conversion plus another backend, authentication and baseline policy. That is deferred rather than duplicating timing reports.

Reconsider Bencher if long-term custom-metric comparisons become worth that integration cost. Automated CPU/RSS gates, profiling pipelines and cloud scale suites are not part of these lightweight checks.

## Related experience

Karpenter core's [regression experiments](https://github.com/kubernetes-sigs/karpenter/blob/40bd9d28733fb41de4a6d6702bd581bf95bd75c5/test/suites/performancev2/perf/regressions/README.md) show that workload benchmarks catch some allocation regressions missed by cluster scenarios. The [AWS instance-type tests](https://github.com/aws/karpenter-provider-aws/blob/516519d5c941a886b7e643672b2bbca8c8bfb4f1/pkg/providers/instancetype/suite_test.go) protect cache invalidation and result ownership. This suite reuses the GCP provider's existing fake pricing/GKE and HTTP-test seams instead of importing a cluster test framework.

Instrument references: [CodSpeed Go](https://codspeed.io/docs/benchmarks/go), [CodSpeed Wizard](https://codspeed.io/docs/ai/wizard), [Bencher adapters](https://bencher.dev/docs/explanation/adapters/).
