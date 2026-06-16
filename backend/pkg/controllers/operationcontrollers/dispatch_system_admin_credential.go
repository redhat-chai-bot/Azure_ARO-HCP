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

package operationcontrollers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	utilsclock "k8s.io/utils/clock"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/systemadmincredential"
	"github.com/Azure/ARO-HCP/internal/utils"
	"github.com/Azure/ARO-HCP/internal/utils/apihelpers"
)

type dispatchSystemAdminCredential struct {
	clock             utilsclock.PassiveClock
	resourcesDBClient database.ResourcesDBClient
}

// NewDispatchSystemAdminCredentialController returns a new Controller instance that
// creates a SystemAdminCredential Cosmos document and generates the keypair for
// a credential request operation.
//
// Operation documents relevant to this controller will have the following values:
//
//	ResourceType: Microsoft.RedHatOpenShift/hcpOpenShiftClusters
//	     Request: RequestCredential
//	      Status: Accepted
//	  InternalID: an empty value
func NewDispatchSystemAdminCredentialController(
	clock utilsclock.PassiveClock,
	resourcesDBClient database.ResourcesDBClient,
	activeOperationInformer cache.SharedIndexInformer,
) controllerutils.Controller {
	syncer := &dispatchSystemAdminCredential{
		clock:             clock,
		resourcesDBClient: resourcesDBClient,
	}

	controller := NewGenericOperationController(
		"DispatchSystemAdminCredential",
		syncer,
		10*time.Second,
		activeOperationInformer,
		resourcesDBClient,
	)

	return controller
}

func (c *dispatchSystemAdminCredential) ShouldProcess(ctx context.Context, operation *api.Operation) bool {
	if operation.Status.IsTerminal() {
		return false
	}
	if operation.Request != database.OperationRequestRequestCredential {
		return false
	}
	if len(operation.InternalID.String()) > 0 {
		return false
	}
	return true
}

func (c *dispatchSystemAdminCredential) SynchronizeOperation(ctx context.Context, key controllerutils.OperationKey) error {
	logger := utils.LoggerFromContext(ctx)
	logger.Info("checking operation")

	operation, err := c.resourcesDBClient.Operations(key.SubscriptionID).Get(ctx, key.OperationName)
	if database.IsNotFoundError(err) {
		return nil // no work to do
	}
	if err != nil {
		return fmt.Errorf("failed to get active operation: %w", err)
	}
	if !c.ShouldProcess(ctx, operation) {
		return nil // no work to do
	}

	cluster, err := c.resourcesDBClient.HCPClusters(operation.ExternalID.SubscriptionID, operation.ExternalID.ResourceGroupName).Get(ctx, operation.ExternalID.Name)
	if err != nil {
		return utils.TrackError(err)
	}

	// Cancel the operation if a revocation is in progress.
	//
	// The frontend cancels all active RequestCredential operations when
	// handling a revocation request, but it cannot do so atomically. So
	// there is a slim chance of a straggler slipping through. This is a
	// second line of defense.
	if len(cluster.ServiceProviderProperties.RevokeCredentialsOperationID) > 0 {
		logger.Info("revocation in progress, canceling operation",
			"revoke_credentials_operation_id", cluster.ServiceProviderProperties.RevokeCredentialsOperationID)

		apihelpers.CancelOperation(operation, c.clock.Now())

		_, err = c.resourcesDBClient.Operations(key.SubscriptionID).Replace(ctx, operation, nil)
		if err != nil {
			return utils.TrackError(err)
		}

		return nil
	}

	// Idempotency: if we already created a credential for this operation
	// (e.g. the InternalID update below failed on a previous attempt),
	// find the existing credential and skip creation.
	credCRUD := c.resourcesDBClient.HCPClusters(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
	).SystemAdminCredentials(operation.ExternalID.Name)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return utils.TrackError(err)
	}
	for cred, iterErr := range iter.Items(ctx) {
		if iterErr != nil {
			return utils.TrackError(iterErr)
		}
		if cred.Spec.OperationID == operation.OperationID.Name {
			logger.Info("found existing credential for this operation, reusing",
				"credential_name", cred.CosmosMetadata.ID)

			credInternalID, idErr := credentialInternalID(cluster, cred.CosmosMetadata.ID)
			if idErr != nil {
				return utils.TrackError(idErr)
			}
			operation.InternalID = credInternalID
			_, err = c.resourcesDBClient.Operations(key.SubscriptionID).Replace(ctx, operation, nil)
			if err != nil {
				return utils.TrackError(err)
			}
			return nil
		}
	}

	// Generate keypair and create a new SystemAdminCredential document.
	credName := systemadmincredential.CredName(uuid.New().String())

	publicPEM, privatePEM, err := systemadmincredential.GenerateKeypair()
	if err != nil {
		return utils.TrackError(err)
	}

	now := c.clock.Now()
	newCred := &api.SystemAdminCredential{
		Spec: api.SystemAdminCredentialSpec{
			Username:            "system:admin",
			ExpirationTimestamp: metav1.NewTime(now.Add(24 * time.Hour)),
			OperationID:         operation.OperationID.Name,
			PublicKeyPEM:        string(publicPEM),
			PrivateKeyPEM:       string(privatePEM),
		},
		Status: api.SystemAdminCredentialStatus{
			Phase: api.SystemAdminCredentialPhaseRequested,
		},
	}
	newCred.CosmosMetadata.ID = credName

	_, err = credCRUD.Create(ctx, newCred, nil)
	if err != nil {
		return utils.TrackError(err)
	}

	// If this operation document update fails then we will abandon the credential
	// created above and start a new credential on the next retry thanks to the
	// idempotency check. The abandoned credential will never reach the Issued phase
	// because no desires will be created for it.
	credInternalID, err := credentialInternalID(cluster, credName)
	if err != nil {
		return utils.TrackError(err)
	}
	operation.InternalID = credInternalID

	_, err = c.resourcesDBClient.Operations(key.SubscriptionID).Replace(ctx, operation, nil)
	if err != nil {
		return utils.TrackError(err)
	}

	logger.Info("created SystemAdminCredential and set InternalID",
		"credential_name", credName)

	return nil
}

// credentialInternalID synthesises an InternalID that matches the existing
// break_glass_credentials pattern so that InternalID.validate() passes.
// The CS cluster ID is available on the cluster document.
func credentialInternalID(cluster *api.HCPOpenShiftCluster, credName string) (api.InternalID, error) {
	if cluster.ServiceProviderProperties.ClusterServiceID == nil {
		return api.InternalID{}, fmt.Errorf("cluster has no ClusterServiceID")
	}
	href := fmt.Sprintf("%s/break_glass_credentials/%s",
		cluster.ServiceProviderProperties.ClusterServiceID.String(), credName)
	return api.NewInternalID(href)
}
