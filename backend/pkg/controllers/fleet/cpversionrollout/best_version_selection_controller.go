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

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/fleetcosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/fleetinformers"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// BestVersionSelectionControllerName is the Cosmos controller document ID for this syncer.
const BestVersionSelectionControllerName = "ControlPlaneVersionBestVersionSelection"

// bestVersionSelectionSyncer selects the best exact version for a channel and
// writes it to ControlPlaneVersionRollout.spec.bestExactVersion. It fires on an
// interval.
type bestVersionSelectionSyncer struct {
	fleetDBClient fleetcosmosstorage.FleetDBClient
	resolver      BestVersionResolver
	config        RolloutConfig
	cooldown      controllerutil.CooldownChecker
}

var _ controllerutils.ControlPlaneVersionRolloutSyncer = (*bestVersionSelectionSyncer)(nil)

// NewBestVersionSelectionController creates the per-rollout controller that
// selects the best exact version for each y-stream channel on an interval.
func NewBestVersionSelectionController(
	fleetDBClient fleetcosmosstorage.FleetDBClient,
	fleetInformers fleetinformers.FleetInformers,
	resolver BestVersionResolver,
	config RolloutConfig,
) controllerutils.Controller {
	syncer := &bestVersionSelectionSyncer{
		fleetDBClient: fleetDBClient,
		resolver:      resolver,
		config:        config,
		cooldown:      controllerutil.NewTimeBasedCooldownChecker(DefaultResyncDuration),
	}
	return controllerutils.NewControlPlaneVersionRolloutWatchingController(
		BestVersionSelectionControllerName,
		fleetDBClient,
		fleetInformers,
		DefaultResyncDuration,
		syncer,
	)
}

func (c *bestVersionSelectionSyncer) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldown
}

func (c *bestVersionSelectionSyncer) SyncOnce(ctx context.Context, key controllerutils.ControlPlaneVersionRolloutKey) error {
	logger := utils.LoggerFromContext(ctx)

	rollouts := c.fleetDBClient.ControlPlaneVersionRollouts()
	existing, err := rollouts.Get(ctx, key.Channel)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil // nothing to do until the rollout document exists
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ControlPlaneVersionRollout %q: %w", key.Channel, err))
	}

	best, err := c.resolver.BestExactVersion(ctx, key.Channel, c.config.ZStreamOffset)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to resolve best exact version for %q: %w", key.Channel, err))
	}
	if best == nil {
		logger.Info("no best exact version available yet", "channel", key.Channel)
		return nil
	}

	// Floor the selection by the SRE-specified minimum version for the channel.
	if minVersion, ok := c.config.MinimumVersions[key.Channel]; ok && best.LT(minVersion) {
		floored := minVersion
		best = &floored
	}

	if existing.Spec.BestExactVersion != nil && existing.Spec.BestExactVersion.EQ(*best) {
		return nil // already at the desired best version
	}

	updated := existing.DeepCopy()
	updated.Spec.BestExactVersion = best
	if _, err := rollouts.Replace(ctx, updated, existing, nil); err != nil {
		return ignoreWriteConflict(err)
	}
	logger.Info("updated best exact version", "channel", key.Channel, "bestExactVersion", best.String())
	return nil
}
