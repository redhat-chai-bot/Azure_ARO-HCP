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
	"net/http"
	"time"

	"k8s.io/client-go/tools/cache"
	utilsclock "k8s.io/utils/clock"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type operationSystemAdminCredential struct {
	clock              utilsclock.PassiveClock
	resourcesDBClient  database.ResourcesDBClient
	notificationClient *http.Client
}

// NewOperationSystemAdminCredentialController returns a new Controller instance that
// follows a SystemAdminCredential through its lifecycle and updates the
// corresponding operation document in Cosmos DB.
//
// Operation documents relevant to this controller will have the following values:
//
//	ResourceType: Microsoft.RedHatOpenShift/hcpOpenShiftClusters
//	     Request: RequestCredential
//	      Status: any non-terminal value
//	  InternalID: a Clusters Service HREF value (containing the credential name)
//
// The controller maps SystemAdminCredential phases to ARM provisioning states:
//
//	Requested          -> Provisioning
//	Issued             -> Succeeded
//	Failed             -> Failed
//	AwaitingRevocation -> (no change, still Provisioning until fully revoked)
func NewOperationSystemAdminCredentialController(
	clock utilsclock.PassiveClock,
	resourcesDBClient database.ResourcesDBClient,
	notificationClient *http.Client,
	activeOperationInformer cache.SharedIndexInformer,
) controllerutils.Controller {
	syncer := &operationSystemAdminCredential{
		clock:              clock,
		resourcesDBClient:  resourcesDBClient,
		notificationClient: notificationClient,
	}

	controller := NewGenericOperationController(
		"OperationSystemAdminCredential",
		syncer,
		10*time.Second,
		activeOperationInformer,
		resourcesDBClient,
	)

	return controller
}

func (c *operationSystemAdminCredential) ShouldProcess(ctx context.Context, operation *api.Operation) bool {
	if operation.Status.IsTerminal() {
		return false
	}
	if operation.Request != database.OperationRequestRequestCredential {
		return false
	}
	if len(operation.InternalID.String()) == 0 {
		return false
	}
	return true
}

func (c *operationSystemAdminCredential) SynchronizeOperation(ctx context.Context, key controllerutils.OperationKey) error {
	logger := utils.LoggerFromContext(ctx)
	logger.Info("checking operation")

	oldOperation, err := c.resourcesDBClient.Operations(key.SubscriptionID).Get(ctx, key.OperationName)
	if database.IsNotFoundError(err) {
		return nil // no work to do
	}
	if err != nil {
		return fmt.Errorf("failed to get active operation: %w", err)
	}
	if !c.ShouldProcess(ctx, oldOperation) {
		return nil // no work to do
	}

	// Look up the SystemAdminCredential by listing under the cluster
	// and matching on OperationID, since InternalID is a synthetic CS
	// path and the credential name is at the tail.
	credCRUD := c.resourcesDBClient.HCPClusters(
		oldOperation.ExternalID.SubscriptionID,
		oldOperation.ExternalID.ResourceGroupName,
	).SystemAdminCredentials(oldOperation.ExternalID.Name)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return utils.TrackError(err)
	}

	var cred *api.SystemAdminCredential
	for _, candidate := range iter.Items(ctx) {
		if candidate.Spec.OperationID == oldOperation.OperationID.Name {
			cred = candidate
			break
		}
	}
	if err := iter.GetError(); err != nil {
		return utils.TrackError(fmt.Errorf("iterating SystemAdminCredentials: %w", err))
	}
	if cred == nil {
		return fmt.Errorf("SystemAdminCredential not found for operation %s", oldOperation.OperationID.Name)
	}

	var newOperationStatus arm.ProvisioningState
	var newOperationError *arm.CloudErrorBody

	switch cred.Status.Phase {
	case api.SystemAdminCredentialPhaseRequested:
		newOperationStatus = arm.ProvisioningStateProvisioning
	case api.SystemAdminCredentialPhaseIssued:
		newOperationStatus = arm.ProvisioningStateSucceeded
	case api.SystemAdminCredentialPhaseFailed:
		newOperationStatus = arm.ProvisioningStateFailed
		newOperationError = &arm.CloudErrorBody{
			Code:    arm.CloudErrorCodeInternalServerError,
			Message: "Failed to provision cluster credential",
		}
	case api.SystemAdminCredentialPhaseAwaitingRevocation:
		// Credential was issued but is now being revoked. From the
		// operation's perspective this is still in-progress.
		newOperationStatus = arm.ProvisioningStateProvisioning
	case api.SystemAdminCredentialPhaseRevoked:
		// Should not happen during the request flow, but treat as failed.
		newOperationStatus = arm.ProvisioningStateFailed
		newOperationError = &arm.CloudErrorBody{
			Code:    arm.CloudErrorCodeInternalServerError,
			Message: "Credential was revoked before issuance completed",
		}
	default:
		return fmt.Errorf("unhandled SystemAdminCredentialPhase %q", cred.Status.Phase)
	}

	if !needToPatchOperation(oldOperation, newOperationStatus, newOperationError) {
		return nil
	}

	err = patchOperation(ctx, c.clock, c.resourcesDBClient, oldOperation, newOperationStatus, newOperationError, postAsyncNotificationFn(c.notificationClient))
	if err != nil {
		return utils.TrackError(err)
	}

	return nil
}
