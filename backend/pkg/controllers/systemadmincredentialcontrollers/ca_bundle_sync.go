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
	"encoding/json"
	"fmt"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// CABundleSync reads the mirrored serving CA Secret content from the
// ReadDesire created by ServingCAReadDesireCreator (controller 10),
// extracts the CA bundle PEM, and writes it to
// ServiceProviderCluster.Status.ServingCABundle. The CA is stored once
// on the cluster doc, not per-credential.
//
// It uses a "compare-then-Replace" pattern: if the observed CA bundle
// differs from what is already stored, the ServiceProviderCluster
// document is updated. Identical values are a no-op.
type CABundleSync struct {
	ResourcesDBClient                   database.ResourcesDBClient
	KubeApplierDBClients                database.KubeApplierDBClients
	HostedClusterNamespaceEnvIdentifier string
}

// NewCABundleSync constructs a CABundleSync.
func NewCABundleSync(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
) *CABundleSync {
	return &CABundleSync{
		ResourcesDBClient:                   resourcesDBClient,
		KubeApplierDBClients:                kubeApplierDBClients,
		HostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}
}

// SyncCluster reads the serving CA ReadDesire for the given cluster,
// extracts the CA bundle from the mirrored Secret, and syncs it to
// ServiceProviderCluster.Status.ServingCABundle if changed.
//
// Returns 1 if the CA bundle was synced (or already up to date), 0 if
// the ReadDesire or its content is not yet available.
func (cb *CABundleSync) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := cb.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
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

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, cb.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return 0, nil
	}

	kaClient := cb.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return 0, nil
	}

	// --- Read the serving CA ReadDesire ---

	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}

	desireName := fmt.Sprintf("serving-ca-%s", clusterName)
	readDesire, err := readCRUD.Get(ctx, desireName)
	if err != nil {
		if database.IsNotFoundError(err) {
			// ReadDesire not yet created by controller 10; nothing to do.
			return 0, nil
		}
		return 0, fmt.Errorf("getting serving CA ReadDesire: %w", err)
	}

	// Check that the kube-applier has mirrored the Secret content.
	if readDesire.Status.KubeContent == nil || readDesire.Status.KubeContent.Raw == nil {
		// Not yet observed; retry on next sync.
		return 0, nil
	}

	// --- Extract the CA bundle from the mirrored Secret ---

	caBundle, err := extractCABundleFromSecret(readDesire.Status.KubeContent.Raw)
	if err != nil {
		return 0, fmt.Errorf("extracting CA bundle from mirrored Secret: %w", err)
	}
	if caBundle == "" {
		// Secret exists but has no CA data — unusual but not fatal.
		logger.Info("serving CA ReadDesire has no CA bundle data",
			"cluster", clusterName, "desire_name", desireName)
		return 0, nil
	}

	// --- Sync to ServiceProviderCluster if changed ---

	if spc.Status.ServingCABundle == caBundle {
		// Already up to date.
		return 1, nil
	}

	spc.Status.ServingCABundle = caBundle

	spcCRUD := cb.ResourcesDBClient.ServiceProviderClusters(
		subscriptionID, resourceGroupName, clusterName)
	if _, err := spcCRUD.Replace(ctx, spc, nil); err != nil {
		return 0, fmt.Errorf("updating ServiceProviderCluster ServingCABundle: %w", err)
	}

	logger.Info("synced serving CA bundle to ServiceProviderCluster",
		"cluster", clusterName,
		"ca_bundle_bytes", len(caBundle))

	return 1, nil
}

// extractCABundleFromSecret unmarshals a mirrored corev1.Secret from its
// JSON representation and returns the CA bundle PEM string. It checks
// the "tls.crt" key first (standard TLS Secret layout), then "ca.crt"
// as a fallback.
func extractCABundleFromSecret(raw []byte) (string, error) {
	var secret corev1.Secret
	if err := json.Unmarshal(raw, &secret); err != nil {
		return "", fmt.Errorf("unmarshaling Secret: %w", err)
	}

	// Try standard key names in order of precedence.
	for _, key := range []string{"tls.crt", "ca.crt"} {
		if data, ok := secret.Data[key]; ok && len(data) > 0 {
			return string(data), nil
		}
	}

	return "", nil
}
