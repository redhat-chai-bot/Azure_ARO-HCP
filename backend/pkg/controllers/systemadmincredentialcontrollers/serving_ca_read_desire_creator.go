// Copyright 2025 Microsoft Corporation
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

package systemadmincredentialcontrollers

import (
	"context"
	"fmt"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	// servingCASecretName is the name of the HyperShift-managed Secret
	// containing the kube-apiserver serving CA certificate in the HCP
	// namespace on the management cluster.
	servingCASecretName = "kas-server-crt"
)

// ServingCAReadDesireCreator creates a long-lived ReadDesire on the
// HyperShift serving CA Secret in the management cluster — one per
// cluster, not per credential. This mirrors the serving CA Secret
// content into Cosmos so controller 8 (CABundleSync) can extract the
// CA bytes and write them onto ServiceProviderClusterStatus.ServingCABundle.
//
// The ReadDesire is idempotent: re-runs skip creation if the desire
// already exists.
type ServingCAReadDesireCreator struct {
	ResourcesDBClient                   database.ResourcesDBClient
	KubeApplierDBClients                database.KubeApplierDBClients
	HostedClusterNamespaceEnvIdentifier string
}

// NewServingCAReadDesireCreator constructs a ServingCAReadDesireCreator.
func NewServingCAReadDesireCreator(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
) *ServingCAReadDesireCreator {
	return &ServingCAReadDesireCreator{
		ResourcesDBClient:                   resourcesDBClient,
		KubeApplierDBClients:                kubeApplierDBClients,
		HostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}
}

// SyncCluster ensures a ReadDesire exists for the serving CA Secret of the
// given cluster. It returns 1 if the ReadDesire was created (or already
// existed) and 0 if prerequisites were not met.
func (sc *ServingCAReadDesireCreator) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := sc.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
	if database.IsNotFoundError(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("getting cluster: %w", err)
	}
	if cluster.ServiceProviderProperties.ClusterServiceID == nil {
		return 0, nil
	}

	clusterResourceID, err := azcorearm.ParseResourceID(
		fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s",
			subscriptionID, resourceGroupName,
			api.ClusterResourceType.String(), clusterName))
	if err != nil {
		return 0, fmt.Errorf("parsing cluster resource ID: %w", err)
	}

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, sc.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return 0, nil
	}

	kaClient := sc.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return 0, nil
	}

	hcpNamespace := fmt.Sprintf("ocm-%s-%s",
		sc.HostedClusterNamespaceEnvIdentifier,
		cluster.ServiceProviderProperties.ClusterServiceID.ID())

	// --- Ensure the serving CA ReadDesire ---

	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}

	// Use a stable deterministic name scoped to this cluster.
	desireName := fmt.Sprintf("serving-ca-%s", clusterName)

	spec := desireToCreate{
		name: desireName,
		kind: api.SystemAdminCredentialDesireKindRead,
		targetItem: kubeapplier.ResourceReference{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: hcpNamespace,
			Name:      servingCASecretName,
		},
	}

	if err := ensureReadDesire(
		ctx, readCRUD, spec,
		subscriptionID, resourceGroupName, clusterName, mcResourceID,
	); err != nil {
		return 0, fmt.Errorf("ensuring serving CA ReadDesire: %w", err)
	}

	logger.Info("ensured serving CA ReadDesire",
		"cluster", clusterName,
		"desire_name", desireName,
		"namespace", hcpNamespace)

	return 1, nil
}
