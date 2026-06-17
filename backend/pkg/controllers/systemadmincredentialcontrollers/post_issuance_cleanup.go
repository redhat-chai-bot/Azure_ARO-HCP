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

// PostIssuanceCleanup tears down CSR/CSRA/RBAC ApplyDesires and the CSR
// ReadDesire from the management cluster once a SystemAdminCredential reaches
// Phase=Issued or Phase=Failed. It does this by issuing a DeleteDesire for
// each outstanding ApplyDesire target and removing the ReadDesire, since the
// underlying k8s objects are no longer needed. The private key and signed
// certificate remain in Cosmos.
//
// For each eligible credential, the controller:
//  1. Iterates over OutstandingDesires of kind ApplyDesire or ReadDesire.
//  2. For ApplyDesire refs, creates a corresponding DeleteDesire targeting the
//     same k8s object (so the kube-applier removes it from the MC).
//  3. For ReadDesire refs, deletes the ReadDesire document directly (there is
//     no corresponding DeleteDesire for reads — just remove the desire).
//  4. Removes the processed ref from OutstandingDesires and updates the
//     credential document.
type PostIssuanceCleanup struct {
	ResourcesDBClient                   database.ResourcesDBClient
	KubeApplierDBClients                database.KubeApplierDBClients
	HostedClusterNamespaceEnvIdentifier string
}

// NewPostIssuanceCleanup constructs a PostIssuanceCleanup.
func NewPostIssuanceCleanup(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
) *PostIssuanceCleanup {
	return &PostIssuanceCleanup{
		ResourcesDBClient:                   resourcesDBClient,
		KubeApplierDBClients:                kubeApplierDBClients,
		HostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}
}

// SyncCluster processes every SystemAdminCredential under the given cluster
// that is in Phase=Issued or Phase=Failed and still has OutstandingDesires.
// It returns the number of credentials cleaned up and the first error
// encountered (after attempting all eligible credentials).
func (pc *PostIssuanceCleanup) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := pc.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
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

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, pc.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return 0, nil
	}

	kaClient := pc.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return 0, nil
	}

	// --- List credentials and process Issued/Failed ones ---

	credCRUD := pc.ResourcesDBClient.HCPClusters(
		subscriptionID, resourceGroupName,
	).SystemAdminCredentials(clusterName)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("listing SystemAdminCredentials: %w", err)
	}

	processed := 0
	var firstErr error
	for _, cred := range iter.Items(ctx) {
		if cred.Status.Phase != api.SystemAdminCredentialPhaseIssued &&
			cred.Status.Phase != api.SystemAdminCredentialPhaseFailed {
			continue
		}
		if len(cred.Status.OutstandingDesires) == 0 {
			continue
		}

		credName := cred.CosmosMetadata.ResourceID.Name
		if err := pc.cleanupDesiresForCredential(
			ctx, credCRUD, cred, kaClient,
			subscriptionID, resourceGroupName, clusterName, mcResourceID,
		); err != nil {
			logger.Error(err, "failed to clean up desires for credential",
				"credential_name", credName)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		processed++
	}
	if iterErr := iter.GetError(); iterErr != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("iterating SystemAdminCredentials: %w", iterErr)
		}
	}

	return processed, firstErr
}

// cleanupDesiresForCredential removes all outstanding ApplyDesires and
// ReadDesires for a single credential that has completed issuance.
func (pc *PostIssuanceCleanup) cleanupDesiresForCredential(
	ctx context.Context,
	credCRUD database.ResourceCRUD[api.SystemAdminCredential],
	cred *api.SystemAdminCredential,
	kaClient database.KubeApplierDBClient,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	logger := utils.LoggerFromContext(ctx)
	credName := cred.CosmosMetadata.ResourceID.Name

	applyCRUD, err := kaClient.ApplyDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return fmt.Errorf("getting ApplyDesire CRUD: %w", err)
	}
	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}
	deleteCRUD, err := kaClient.DeleteDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return fmt.Errorf("getting DeleteDesire CRUD: %w", err)
	}

	// Process each outstanding desire. We build a new slice of refs that
	// could not be cleaned up (due to errors) so the credential doc stays
	// accurate.
	var remaining []api.SystemAdminCredentialDesireRef

	for _, ref := range cred.Status.OutstandingDesires {
		switch ref.Kind {
		case api.SystemAdminCredentialDesireKindApply:
			if err := pc.cleanupApplyDesire(
				ctx, applyCRUD, deleteCRUD, ref,
				subscriptionID, resourceGroupName, clusterName, mcResourceID,
			); err != nil {
				logger.Error(err, "failed to clean up ApplyDesire",
					"credential_name", credName, "desire_name", ref.Name)
				remaining = append(remaining, ref)
				continue
			}

		case api.SystemAdminCredentialDesireKindRead:
			if err := pc.cleanupReadDesire(ctx, readCRUD, ref); err != nil {
				logger.Error(err, "failed to clean up ReadDesire",
					"credential_name", credName, "desire_name", ref.Name)
				remaining = append(remaining, ref)
				continue
			}

		default:
			// Keep refs of unknown/other kinds (e.g. DeleteDesire refs
			// that may have been added by this controller on a prior run).
			remaining = append(remaining, ref)
			continue
		}
	}

	cred.Status.OutstandingDesires = remaining
	if _, err := credCRUD.Replace(ctx, cred, nil); err != nil {
		return fmt.Errorf("updating credential OutstandingDesires: %w", err)
	}

	logger.Info("cleaned up desires for credential",
		"credential_name", credName,
		"remaining_desires", len(remaining))

	return nil
}

// cleanupApplyDesire handles a single ApplyDesire ref: it looks up the
// original ApplyDesire to get the target reference, creates a DeleteDesire
// for the same target (so the kube-applier removes the k8s object), and
// then deletes the ApplyDesire document.
func (pc *PostIssuanceCleanup) cleanupApplyDesire(
	ctx context.Context,
	applyCRUD database.ResourceCRUD[kubeapplier.ApplyDesire],
	deleteCRUD database.ResourceCRUD[kubeapplier.DeleteDesire],
	ref api.SystemAdminCredentialDesireRef,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	// Look up the existing ApplyDesire to get its TargetItem.
	applyDesire, err := applyCRUD.Get(ctx, ref.Name)
	if err != nil && !database.IsNotFoundError(err) {
		return fmt.Errorf("getting ApplyDesire %s: %w", ref.Name, err)
	}

	if applyDesire != nil {
		// Create a DeleteDesire targeting the same k8s object.
		if err := ensureDeleteDesire(
			ctx, deleteCRUD, ref.Name,
			applyDesire.Spec.TargetItem,
			subscriptionID, resourceGroupName, clusterName, mcResourceID,
		); err != nil {
			return fmt.Errorf("creating DeleteDesire for %s: %w", ref.Name, err)
		}

		// Remove the ApplyDesire document now that we've issued a delete.
		if err := applyCRUD.Delete(ctx, ref.Name); err != nil {
			if !database.IsNotFoundError(err) {
				return fmt.Errorf("deleting ApplyDesire %s: %w", ref.Name, err)
			}
		}
	}
	// If the ApplyDesire was already gone (NotFound), the cleanup is a no-op
	// — the ref is still removed from OutstandingDesires.

	return nil
}

// cleanupReadDesire deletes a ReadDesire document directly. ReadDesires don't
// need a corresponding DeleteDesire — removing the desire stops the
// kube-applier from watching the object.
func (pc *PostIssuanceCleanup) cleanupReadDesire(
	ctx context.Context,
	readCRUD database.ResourceCRUD[kubeapplier.ReadDesire],
	ref api.SystemAdminCredentialDesireRef,
) error {
	if err := readCRUD.Delete(ctx, ref.Name); err != nil {
		if database.IsNotFoundError(err) {
			return nil // already gone
		}
		return fmt.Errorf("deleting ReadDesire %s: %w", ref.Name, err)
	}
	return nil
}

// ensureDeleteDesire creates a DeleteDesire if it doesn't already exist.
// ConflictError from a concurrent Create is treated as success.
func ensureDeleteDesire(
	ctx context.Context,
	crud database.ResourceCRUD[kubeapplier.DeleteDesire],
	name string,
	targetItem kubeapplier.ResourceReference,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	// Check if already exists.
	existing, err := crud.Get(ctx, name)
	if err != nil && !database.IsNotFoundError(err) {
		return fmt.Errorf("checking existing: %w", err)
	}
	if existing != nil {
		return nil // already exists
	}

	resourceIDStr := kubeapplier.ToClusterScopedDeleteDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, name)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return fmt.Errorf("parsing desire resource ID: %w", err)
	}

	desire := &kubeapplier.DeleteDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.DeleteDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem:        targetItem,
		},
	}

	if _, err := crud.Create(ctx, desire, nil); err != nil {
		if database.IsConflictError(err) {
			return nil // concurrent creation, treat as success
		}
		return fmt.Errorf("creating: %w", err)
	}
	return nil
}
