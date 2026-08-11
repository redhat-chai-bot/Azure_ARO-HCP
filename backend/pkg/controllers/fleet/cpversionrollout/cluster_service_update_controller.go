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

	"github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// ClusterServiceUpdateControllerName is the Cosmos controller document ID for this syncer.
const ClusterServiceUpdateControllerName = "ControlPlaneClusterServiceUpdate"

// clusterServiceUpdateSyncer propagates the resolved desired control plane
// version onto the desired HostedCluster's control plane release image. In the
// design this is "logical" and ultimately owned by cluster-service; here we mirror
// the desired version onto ServiceProviderCluster.Spec.DesiredHostedCluster so the
// downstream dispatch/cluster-service integration has a single source of truth.
type clusterServiceUpdateSyncer struct {
	resourcesDBClient            corecosmosstorage.ResourcesDBClient
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
}

var _ controllerutils.ClusterSyncer = (*clusterServiceUpdateSyncer)(nil)

// NewClusterServiceUpdateController creates the per-cluster controller that
// propagates the desired control plane version to the HostedCluster release image.
func NewClusterServiceUpdateController(
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	informers coreinformers.BackendInformers,
) controllerutils.Controller {
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	syncer := &clusterServiceUpdateSyncer{
		resourcesDBClient:            resourcesDBClient,
		serviceProviderClusterLister: serviceProviderClusterLister,
	}
	return controllerutils.NewClusterWatchingController(
		ClusterServiceUpdateControllerName,
		resourcesDBClient,
		informers,
		nil,
		1*time.Minute,
		syncer,
	)
}

func (c *clusterServiceUpdateSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	spc, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderCluster: %w", err))
	}

	desired := spc.Spec.ControlPlaneVersion.DesiredVersion
	if desired == nil {
		return nil // nothing resolved yet
	}
	if spc.Spec.DesiredHostedCluster == nil {
		return nil // the HostedCluster spec is created elsewhere; nothing to update yet
	}

	image := releaseImage(*desired)
	if spc.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease != nil &&
		spc.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease.Image == image {
		return nil
	}

	updated := spc.DeepCopy()
	if updated.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease == nil {
		updated.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease = &v1beta1.Release{}
	}
	updated.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease.Image = image

	_, err = c.resourcesDBClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(ctx, updated, nil)
	return ignoreWriteConflict(err)
}

// releaseImage builds the OCP release payload pullspec for an exact version.
func releaseImage(version semver.Version) string {
	return fmt.Sprintf("quay.io/openshift-release-dev/ocp-release:%s-x86_64", version.String())
}
