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

// ClusterDeletionCleanup is a cluster-deletion gate that ensures ALL
// credential-related desires (ApplyDesires + ReadDesires) for every
// SystemAdminCredential of a cluster are torn down from the management
// cluster before cluster deletion proceeds.
//
// For each credential (regardless of phase) that still has OutstandingDesires:
//  1. ApplyDesire refs get a corresponding DeleteDesire issued (so the
//     kube-applier removes the k8s object from the MC), then the ApplyDesire
//     document is deleted.
//  2. ReadDesire refs are deleted directly.
//  3. DeleteDesire refs are checked for completion — if the DeleteDesire's
//     "Successful" condition is True, the ref is removed; otherwise it is
//     retained so the controller retries on the next sync.
//
// The returned count is the number of credentials that still have
// outstanding desires after this pass. A return of 0 means the cluster
// is fully cleaned and deletion may proceed.
type ClusterDeletionCleanup struct {
	ResourcesDBClient                   database.ResourcesDBClient
	KubeApplierDBClients                database.KubeApplierDBClients
	HostedClusterNamespaceEnvIdentifier string
}

// NewClusterDeletionCleanup constructs a ClusterDeletionCleanup.
func NewClusterDeletionCleanup(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
) *ClusterDeletionCleanup {
	return &ClusterDeletionCleanup{
		ResourcesDBClient:                   resourcesDBClient,
		KubeApplierDBClients:                kubeApplierDBClients,
		HostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}
}

// SyncCluster processes every SystemAdminCredential under the given cluster
// that still has OutstandingDesires, regardless of phase. It issues
// DeleteDesires for ApplyDesire refs, removes ReadDesire documents, and
// checks DeleteDesire refs for completion.
//
// The returned int is the number of credentials that still have unresolved
// outstanding desires. When this reaches 0 the cluster's credential content
// is fully cleaned up and deletion may proceed.
func (cc *ClusterDeletionCleanup) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := cc.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
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

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, cc.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return 0, nil
	}

	kaClient := cc.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return 0, nil
	}

	// --- List credentials and clean up all with outstanding desires ---

	credCRUD := cc.ResourcesDBClient.HCPClusters(
		subscriptionID, resourceGroupName,
	).SystemAdminCredentials(clusterName)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("listing SystemAdminCredentials: %w", err)
	}

	stillOutstanding := 0
	var firstErr error
	for _, cred := range iter.Items(ctx) {
		if len(cred.Status.OutstandingDesires) == 0 {
			continue
		}

		credName := cred.CosmosMetadata.ResourceID.Name
		remaining, err := cc.cleanupAllDesires(
			ctx, credCRUD, cred, kaClient,
			subscriptionID, resourceGroupName, clusterName, mcResourceID,
		)
		if err != nil {
			logger.Error(err, "failed to clean up desires for credential during cluster deletion",
				"credential_name", credName)
			if firstErr == nil {
				firstErr = err
			}
			stillOutstanding++
			continue
		}
		if remaining > 0 {
			stillOutstanding++
		}
	}
	if iterErr := iter.GetError(); iterErr != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("iterating SystemAdminCredentials: %w", iterErr)
		}
	}

	return stillOutstanding, firstErr
}

// cleanupAllDesires processes all outstanding desires for a single credential
// during cluster deletion. Returns the number of desires still remaining
// after this pass (e.g. DeleteDesires not yet confirmed).
func (cc *ClusterDeletionCleanup) cleanupAllDesires(
	ctx context.Context,
	credCRUD database.ResourceCRUD[api.SystemAdminCredential],
	cred *api.SystemAdminCredential,
	kaClient database.KubeApplierDBClient,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)
	credName := cred.CosmosMetadata.ResourceID.Name

	applyCRUD, err := kaClient.ApplyDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting ApplyDesire CRUD: %w", err)
	}
	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}
	deleteCRUD, err := kaClient.DeleteDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting DeleteDesire CRUD: %w", err)
	}

	var remaining []api.SystemAdminCredentialDesireRef

	for _, ref := range cred.Status.OutstandingDesires {
		switch ref.Kind {
		case api.SystemAdminCredentialDesireKindApply:
			if err := cleanupApplyDesireForDeletion(
				ctx, applyCRUD, deleteCRUD, ref,
				subscriptionID, resourceGroupName, clusterName, mcResourceID,
			); err != nil {
				logger.Error(err, "failed to clean up ApplyDesire during cluster deletion",
					"credential_name", credName, "desire_name", ref.Name)
				remaining = append(remaining, ref)
				continue
			}
			// The ApplyDesire is gone and a DeleteDesire was issued.
			// Track the DeleteDesire ref so we can confirm completion.
			remaining = append(remaining, api.SystemAdminCredentialDesireRef{
				Kind: api.SystemAdminCredentialDesireKindDelete,
				Name: ref.Name,
			})

		case api.SystemAdminCredentialDesireKindRead:
			if err := readCRUD.Delete(ctx, ref.Name); err != nil {
				if !database.IsNotFoundError(err) {
					logger.Error(err, "failed to delete ReadDesire during cluster deletion",
						"credential_name", credName, "desire_name", ref.Name)
					remaining = append(remaining, ref)
				}
			}
			// ReadDesire deleted (or was already gone) — ref is removed.

		case api.SystemAdminCredentialDesireKindDelete:
			// Check if the DeleteDesire has completed (target object is gone).
			done, err := isDeleteDesireComplete(ctx, deleteCRUD, ref.Name)
			if err != nil {
				logger.Error(err, "failed to check DeleteDesire completion",
					"credential_name", credName, "desire_name", ref.Name)
				remaining = append(remaining, ref)
				continue
			}
			if !done {
				remaining = append(remaining, ref)
			}
			// If done, the ref drops out of remaining.

		default:
			remaining = append(remaining, ref)
		}
	}

	cred.Status.OutstandingDesires = remaining
	if _, err := credCRUD.Replace(ctx, cred, nil); err != nil {
		return 0, fmt.Errorf("updating credential OutstandingDesires: %w", err)
	}

	logger.Info("cluster deletion cleanup pass for credential",
		"credential_name", credName,
		"remaining_desires", len(remaining))

	return len(remaining), nil
}

// cleanupApplyDesireForDeletion handles a single ApplyDesire ref during
// cluster deletion: looks up the ApplyDesire to get its target, creates a
// DeleteDesire for the same target, and removes the ApplyDesire document.
func cleanupApplyDesireForDeletion(
	ctx context.Context,
	applyCRUD database.ResourceCRUD[kubeapplier.ApplyDesire],
	deleteCRUD database.ResourceCRUD[kubeapplier.DeleteDesire],
	ref api.SystemAdminCredentialDesireRef,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	applyDesire, err := applyCRUD.Get(ctx, ref.Name)
	if err != nil && !database.IsNotFoundError(err) {
		return fmt.Errorf("getting ApplyDesire %s: %w", ref.Name, err)
	}

	if applyDesire != nil {
		if err := ensureDeleteDesire(
			ctx, deleteCRUD, ref.Name,
			applyDesire.Spec.TargetItem,
			subscriptionID, resourceGroupName, clusterName, mcResourceID,
		); err != nil {
			return fmt.Errorf("creating DeleteDesire for %s: %w", ref.Name, err)
		}

		if err := applyCRUD.Delete(ctx, ref.Name); err != nil {
			if !database.IsNotFoundError(err) {
				return fmt.Errorf("deleting ApplyDesire %s: %w", ref.Name, err)
			}
		}
	}

	return nil
}

// isDeleteDesireComplete checks whether a DeleteDesire has finished — the
// target k8s object has been removed from the management cluster. It returns
// true if the DeleteDesire's "Successful" condition is True, or if the
// DeleteDesire document no longer exists (already cleaned up).
func isDeleteDesireComplete(
	ctx context.Context,
	crud database.ResourceCRUD[kubeapplier.DeleteDesire],
	name string,
) (bool, error) {
	desire, err := crud.Get(ctx, name)
	if err != nil {
		if database.IsNotFoundError(err) {
			return true, nil // already gone — treat as complete
		}
		return false, fmt.Errorf("getting DeleteDesire %s: %w", name, err)
	}

	for _, c := range desire.Status.Conditions {
		if c.Type == kubeapplier.ConditionTypeSuccessful &&
			c.Status == "True" {
			// Target is gone. Clean up the DeleteDesire document itself.
			if err := crud.Delete(ctx, name); err != nil && !database.IsNotFoundError(err) {
				return false, fmt.Errorf("deleting completed DeleteDesire %s: %w", name, err)
			}
			return true, nil
		}
	}

	return false, nil
}
