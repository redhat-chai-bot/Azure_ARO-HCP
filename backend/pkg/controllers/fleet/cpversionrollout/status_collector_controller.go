// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cpversionrollout

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/fleetcosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/fleetinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// StatusCollectorControllerName is the Cosmos controller document ID for this syncer.
const StatusCollectorControllerName = "ControlPlaneVersionStatusCollector"

// statusCollectorSyncer recomputes the ControlPlaneVersionRollout status count
// maps across all ServiceProviderClusters in the channel.
type statusCollectorSyncer struct {
	fleetDBClient                fleetcosmosstorage.FleetDBClient
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
	config                       RolloutConfig
	cooldown                     controllerutil.CooldownChecker
}

var _ controllerutils.ControlPlaneVersionRolloutSyncer = (*statusCollectorSyncer)(nil)

// NewStatusCollectorController creates the per-rollout controller that recomputes
// the rollout status count maps.
func NewStatusCollectorController(
	fleetDBClient fleetcosmosstorage.FleetDBClient,
	fleetInformers fleetinformers.FleetInformers,
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister,
	config RolloutConfig,
) controllerutils.Controller {
	syncer := &statusCollectorSyncer{
		fleetDBClient:                fleetDBClient,
		serviceProviderClusterLister: serviceProviderClusterLister,
		config:                       config,
		cooldown:                     controllerutil.NewTimeBasedCooldownChecker(DefaultResyncDuration),
	}
	return controllerutils.NewControlPlaneVersionRolloutWatchingController(
		StatusCollectorControllerName,
		fleetDBClient,
		fleetInformers,
		DefaultResyncDuration,
		syncer,
	)
}

func (c *statusCollectorSyncer) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldown
}

func (c *statusCollectorSyncer) SyncOnce(ctx context.Context, key controllerutils.ControlPlaneVersionRolloutKey) error {
	rollouts := c.fleetDBClient.ControlPlaneVersionRollouts()
	existing, err := rollouts.Get(ctx, key.Channel)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ControlPlaneVersionRollout %q: %w", key.Channel, err))
	}

	spcs, err := c.serviceProviderClusterLister.List(ctx)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to list ServiceProviderClusters: %w", err))
	}

	counts := collectCounts(spcs, ChannelMinor(key.Channel))

	updated := existing.DeepCopy()
	updated.Status.ClusterCountByDesiredExactVersion = counts.ClusterCountByDesiredExactVersion
	updated.Status.MismatchedClusterCountByDesiredExactVersion = counts.MismatchedClusterCountByDesiredExactVersion
	updated.Status.FailedClusterCountByDesiredExactVersion = counts.FailedClusterCountByDesiredExactVersion
	updated.Status.ClusterCountByAchievedExactVersion = counts.ClusterCountByAchievedExactVersion
	updated.Status.SuccessfulClusterCountByAchievedExactVersion = counts.SuccessfulClusterCountByAchievedExactVersion

	if equality.Semantic.DeepEqual(existing.Status, updated.Status) {
		return nil
	}
	if _, err := rollouts.Replace(ctx, updated, existing, nil); err != nil {
		return ignoreWriteConflict(err)
	}
	return nil
}

// collectCounts recomputes the five rollout status count maps from the clusters
// whose desired version is in the given minor. It is a pure function to keep the
// counting logic directly unit-testable.
//
// Note: FailedClusterCountByDesiredExactVersion uses a Degraded=True condition as
// a proxy for "stuck past maxUpgradeDuration", and
// SuccessfulClusterCountByAchievedExactVersion currently equals the achieved
// count (minVersionReadyDuration gating requires a per-version ready timestamp
// that does not yet exist on ServiceProviderCluster). See the plan doc.
func collectCounts(spcs []*coreapi.ServiceProviderCluster, minor string) fleetapi.ControlPlaneVersionRolloutStatus {
	status := fleetapi.ControlPlaneVersionRolloutStatus{
		ClusterCountByDesiredExactVersion:            map[string]int64{},
		MismatchedClusterCountByDesiredExactVersion:  map[string]int64{},
		FailedClusterCountByDesiredExactVersion:      map[string]int64{},
		ClusterCountByAchievedExactVersion:           map[string]int64{},
		SuccessfulClusterCountByAchievedExactVersion: map[string]int64{},
	}

	for _, spc := range spcs {
		desired := spc.Spec.ControlPlaneVersion.DesiredVersion
		if desired == nil || versionMinor(*desired) != minor {
			continue
		}
		desiredStr := desired.String()
		status.ClusterCountByDesiredExactVersion[desiredStr]++

		if clusterAchievedDesired(spc) {
			status.ClusterCountByAchievedExactVersion[desiredStr]++
			status.SuccessfulClusterCountByAchievedExactVersion[desiredStr]++
			continue
		}

		status.MismatchedClusterCountByDesiredExactVersion[desiredStr]++
		if isDegraded(spc) {
			status.FailedClusterCountByDesiredExactVersion[desiredStr]++
		}
	}

	return status
}
