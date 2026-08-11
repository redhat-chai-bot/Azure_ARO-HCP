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
	"time"

	"github.com/blang/semver/v4"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/informers/fleetinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/database/listers/fleetlisters"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// ForcedDesiredVersionAssignmentControllerName is the Cosmos controller document ID for this syncer.
const ForcedDesiredVersionAssignmentControllerName = "ForcedControlPlaneDesiredVersionAssignment"

// forcedDesiredVersionAssignmentSyncer applies SRE pin overrides to a cluster's
// desired control plane version. When the channel's best version has caught up to
// the pin's expiry, the pin is cleared and the best version adopted; otherwise the
// cluster is held at the pinned exact version.
type forcedDesiredVersionAssignmentSyncer struct {
	resourcesDBClient            corecosmosstorage.ResourcesDBClient
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
	rolloutLister                fleetlisters.ControlPlaneVersionRolloutLister
}

var _ controllerutils.ClusterSyncer = (*forcedDesiredVersionAssignmentSyncer)(nil)

// NewForcedDesiredVersionAssignmentController creates the per-cluster controller
// that enforces SRE version pins.
func NewForcedDesiredVersionAssignmentController(
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	informers coreinformers.BackendInformers,
	fleetInformers fleetinformers.FleetInformers,
) controllerutils.Controller {
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	_, rolloutLister := fleetInformers.ControlPlaneVersionRollouts()
	syncer := &forcedDesiredVersionAssignmentSyncer{
		resourcesDBClient:            resourcesDBClient,
		serviceProviderClusterLister: serviceProviderClusterLister,
		rolloutLister:                rolloutLister,
	}
	return controllerutils.NewClusterWatchingController(
		ForcedDesiredVersionAssignmentControllerName,
		resourcesDBClient,
		informers,
		nil,
		1*time.Minute,
		syncer,
	)
}

func (c *forcedDesiredVersionAssignmentSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	existingCluster, err := c.resourcesDBClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Get(ctx, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster: %w", err))
	}
	if existingCluster.ServiceProviderProperties.DeletionTimestamp != nil {
		return nil
	}

	spc, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderCluster: %w", err))
	}

	pin := spc.Spec.PinnedVersion
	if pin == nil {
		return nil // no pin in effect; the normal assignment controller owns desired version
	}

	channel := fmt.Sprintf("%s-%s", existingCluster.CustomerProperties.Version.ChannelGroup, existingCluster.CustomerProperties.Version.ID)
	rollout, err := c.rolloutLister.Get(ctx, channel)
	if err != nil && !cosmosstorageutils.IsNotFoundError(err) {
		return utils.TrackError(fmt.Errorf("failed to get ControlPlaneVersionRollout %q: %w", channel, err))
	}
	var best *semver.Version
	if rollout != nil && rollout.Spec.BestExactVersion != nil {
		best = rollout.Spec.BestExactVersion
	}

	desired := spc.Spec.ControlPlaneVersion.DesiredVersion

	// The pin has expired: the channel's best version has caught up to the pin's
	// expiry. Adopt the best version and clear the pin.
	if best != nil && pin.UntilExactVersion != nil && best.GE(*pin.UntilExactVersion) {
		updated := spc.DeepCopy()
		updated.Spec.ControlPlaneVersion.DesiredVersion = best
		updated.Spec.PinnedVersion = nil
		_, err := c.resourcesDBClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(ctx, updated, nil)
		return ignoreWriteConflict(err)
	}

	// The pin is still in effect: hold the cluster at the pinned exact version.
	if pin.ExactVersion != nil && (desired == nil || !desired.EQ(*pin.ExactVersion)) {
		updated := spc.DeepCopy()
		exact := *pin.ExactVersion
		updated.Spec.ControlPlaneVersion.DesiredVersion = &exact
		_, err := c.resourcesDBClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(ctx, updated, nil)
		return ignoreWriteConflict(err)
	}

	return nil
}
