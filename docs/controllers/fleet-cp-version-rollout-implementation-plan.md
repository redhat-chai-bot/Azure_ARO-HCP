# Fleet Control Plane Version Rollout — Implementation Plan

This document describes the implementation of the fleet-level control plane
version rollout controllers described in
[`fleet-control-plane-version-rollout.md`](./fleet-control-plane-version-rollout.md).

The goal is to roll out control plane (OpenShift) z-stream versions across the
fleet safely: pick a "best" exact version per y-stream channel, then advance
clusters toward it in canary → rolling waves while respecting SRE pins and a
failure budget.

## New API types

### `fleetapi.ControlPlaneVersionRollout` (region-wide, one per y-stream channel)

A top-level fleet resource stored in the `Fleet` Cosmos container. The document
name is the y-stream channel it is associated with (e.g. `stable-4.21`).

- `Spec.BestExactVersion *semver.Version` — the most recent risk-free z-stream in
  the channel, offset by `zStreamOffset` from the latest available z-stream.
- `Status` — five count maps keyed by exact version string:
  - `ClusterCountByDesiredExactVersion`
  - `MismatchedClusterCountByDesiredExactVersion`
  - `FailedClusterCountByDesiredExactVersion`
  - `ClusterCountByAchievedExactVersion`
  - `SuccessfulClusterCountByAchievedExactVersion`
  - plus `Conditions []metav1.Condition` (standard).

Resource type: `Microsoft.RedHatOpenShift/controlPlaneVersionRollouts`.
Partition key: the lowercased channel name (top-level fleet partitioning).

### `coreapi.ServiceProviderCluster.Spec.PinnedVersion` (new field)

`*ClusterPinnedVersion` with `ExactVersion` and `UntilExactVersion` (`*semver.Version`).
When set, SRE has pinned this cluster to an exact z-stream until
`UntilExactVersion` becomes available in the channel.

## Package layout

| Path | Contents |
|------|----------|
| `internal/api/fleetapi/types_control_plane_version_rollout.go` | new type + spec/status + conditions |
| `internal/api/fleetapi/partition.go` | `GetChannel()` accessor |
| `internal/api/fleetapi/registry.go` | resource type registration |
| `internal/api/fleetapi/types_cosmosdata.go` | resource-ID helpers |
| `internal/api/coreapi/types_serviceprovider_cluster.go` | `PinnedVersion` field + `ClusterPinnedVersion` |
| `internal/validation/validate_control_plane_version_rollout.go` | create/update validation |
| `internal/database/cosmosstorage/fleetcosmosstorage/fleet_client.go` | CRUD + global lister |
| `internal/database/listers/fleetlisters/control_plane_version_rollout_lister.go` | informer-backed lister |
| `internal/database/informers/fleetinformers/*` | informer + wiring |
| `internal/database/cosmosstoragetesting/fleetcosmosstoragetesting/mock_fleet_client.go` | in-memory mock |
| `internal/database/listertesting/fleetlistertesting/slice_listers.go` | slice-backed test lister |
| `backend/pkg/utils/controllerutils/control_plane_version_rollout_watching_controller.go` | per-rollout watching controller + key + syncer |
| `backend/pkg/controllers/fleet/cpversionrollout/*.go` | the five controllers + tests |
| `backend/pkg/app/backend.go` | controller registration |

## The five controllers (all run in the `backend` binary)

1. **Best Version Selection** (`best_version_selection_controller.go`) — per-rollout,
   fires on an interval. Resolves the newest risk-free z-stream for the channel
   from Cincinnati, applies `zStreamOffset`, floors it by the SRE-configured
   `minimumVersions[channel]`, and writes `Spec.BestExactVersion`.
   Depends on a `BestVersionResolver` interface (mockable; real impl backed by the
   Cincinnati graph client) and a `RolloutConfig`.

2. **Status Collector** (`status_collector_controller.go`) — per-rollout. Lists all
   `ServiceProviderCluster`s in the channel and recomputes the five `Status` count
   maps from their desired/active versions and the failure/ready durations.

3. **Normal Cluster Desired Version Assignment** (`normal_desired_version_assignment_controller.go`) —
   per-rollout. The rollout engine: gates on the failure budget (>2 or >5% failed),
   computes `EligibleClusters`, then advances clusters in canary → progressing →
   rolling waves toward `BestExactVersion`, emitting failure/stable/progressing
   conditions.

4. **Forced Cluster Desired Version Assignment** (`forced_desired_version_assignment_controller.go`) —
   per-cluster. Applies SRE pin overrides: when `BestExactVersion >= pin.UntilExactVersion`,
   set desired = best and clear the pin; otherwise honor the pin's `ExactVersion`.

5. **Cluster Service Update/Install** (`cluster_service_update_controller.go`) —
   per-cluster. Propagates `ServiceProviderCluster.Spec.ControlPlaneVersion.DesiredVersion`
   to the HostedCluster control plane release image (logically via cluster-service;
   here the desired version is mirrored onto the desired HostedCluster spec).

Per-rollout controllers use a new `ControlPlaneVersionRolloutWatchingController`
(`Syncer` + `genericWatchingController`) mirroring the existing
`ManagementClusterWatchingController`. Per-cluster controllers reuse the existing
`NewClusterWatchingController`.

## Dependencies

- `github.com/blang/semver/v4` for version comparison (already used by `coreapi`).
- `internal/cincinnati` graph client for channel/version resolution.
- Existing controller framework in `backend/pkg/utils/controllerutils`
  (`GenericSyncer`, `WriteController`, `CooldownChecker`, cosmos CRUD/listers/informers).
- `k8s.io/apimachinery` condition helpers (`meta.SetStatusCondition`).

## Testing strategy

- Same-package, table-driven tests using `testify` (`require`/`assert`).
- Cosmos faked with `fleetcosmosstoragetesting.NewMockFleetDBClient` +
  `corecosmosstoragetesting.NewMockResourcesDBClientWithResources`.
- Listers faked with `fleetlistertesting.Slice*Lister` and
  `corelistertesting.Slice*Lister` / `DB*Lister`.
- External resolvers (Cincinnati best-version) mocked behind a small interface.
- Deterministic clock via `k8s.io/utils/clock/testing`.
- Condition assertions via `meta.FindStatusCondition` (Status/Reason/Message).
- Selection that the design specifies as "random" is made deterministic in tests
  by sorting eligible clusters by name before choosing N.

## Open questions / assumptions

- **Config source** for `Code.ControlPlaneUpgradeController.*` (`minimumVersions`,
  `maxUpgradeDuration`, `minVersionReadyDuration`, `canaryPercentage`,
  `rollingPercentage`) and `zStreamOffset` — modeled here as a `RolloutConfig`
  struct with documented defaults; real plumbing (config/CRD) is TBD.
- **Cincinnati "best version"** resolution is abstracted behind `BestVersionResolver`;
  the production wiring to the graph/risk data is a follow-up.
- **Canary/rolling selection** is specified as random; implemented as deterministic
  (name-sorted) selection to keep it reviewable and testable, with a seam to swap
  in richer criteria later.
- **Cosmos data flow doc** (`docs/cosmos-data-flow.md`) and field `// Written by:`
  annotations must be updated as the controllers land.
