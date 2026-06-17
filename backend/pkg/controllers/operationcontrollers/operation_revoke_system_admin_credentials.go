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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	utilsclock "k8s.io/utils/clock"

	hypershiftcertv1alpha1 "github.com/openshift/hypershift/api/certificates/v1alpha1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/systemadmincredential"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type operationRevokeSystemAdminCredentials struct {
	clock                utilsclock.PassiveClock
	resourcesDBClient    database.ResourcesDBClient
	kubeApplierDBClients database.KubeApplierDBClients
	notificationClient   *http.Client

	hostedClusterNamespaceEnvIdentifier string
}

// NewOperationRevokeSystemAdminCredentialsController returns a Controller that
// polls the CRR ReadDesire until PreviousCertificatesRevoked is True, then
// marks every AwaitingRevocation credential as Revoked and completes the
// operation.
//
// Operation documents relevant to this controller will have the following values:
//
//	ResourceType: Microsoft.RedHatOpenShift/hcpOpenShiftClusters
//	     Request: RevokeCredentials
//	      Status: Deleting
func NewOperationRevokeSystemAdminCredentialsController(
	clock utilsclock.PassiveClock,
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	notificationClient *http.Client,
	hostedClusterNamespaceEnvIdentifier string,
	activeOperationInformer cache.SharedIndexInformer,
) controllerutils.Controller {
	syncer := &operationRevokeSystemAdminCredentials{
		clock:                               clock,
		resourcesDBClient:                   resourcesDBClient,
		kubeApplierDBClients:                kubeApplierDBClients,
		notificationClient:                  notificationClient,
		hostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}

	controller := NewGenericOperationController(
		"OperationRevokeSystemAdminCredentials",
		syncer,
		10*time.Second,
		activeOperationInformer,
		resourcesDBClient,
	)

	return controller
}

func (c *operationRevokeSystemAdminCredentials) ShouldProcess(ctx context.Context, operation *api.Operation) bool {
	if operation.Status.IsTerminal() {
		return false
	}
	if operation.Request != database.OperationRequestRevokeCredentials {
		return false
	}
	if operation.Status == arm.ProvisioningStateAccepted {
		return false
	}
	return true
}

func (c *operationRevokeSystemAdminCredentials) SynchronizeOperation(ctx context.Context, key controllerutils.OperationKey) error {
	logger := utils.LoggerFromContext(ctx)
	logger.Info("checking operation")

	oldOperation, err := c.resourcesDBClient.Operations(key.SubscriptionID).Get(ctx, key.OperationName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get active operation: %w", err)
	}
	if !c.ShouldProcess(ctx, oldOperation) {
		return nil
	}

	// Check whether the CRR on the management cluster has completed.
	revocationComplete, err := c.isCRRComplete(ctx, oldOperation)
	if err != nil {
		return utils.TrackError(err)
	}
	if !revocationComplete {
		// Still in progress — no state change.
		return nil
	}

	// CRR is complete: mark every AwaitingRevocation credential as Revoked.
	now := c.clock.Now()
	credCRUD := c.resourcesDBClient.HCPClusters(
		oldOperation.ExternalID.SubscriptionID,
		oldOperation.ExternalID.ResourceGroupName,
	).SystemAdminCredentials(oldOperation.ExternalID.Name)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return utils.TrackError(err)
	}
	for _, cred := range iter.Items(ctx) {
		if cred.Status.Phase != api.SystemAdminCredentialPhaseAwaitingRevocation {
			continue
		}
		cred.Status.Phase = api.SystemAdminCredentialPhaseRevoked
		revokedAt := metav1.NewTime(now)
		cred.Status.RevokedAt = &revokedAt
		// Zero out the private key — it is no longer needed and reduces
		// the blast radius of a Cosmos read.
		cred.Spec.PrivateKeyPEM = ""

		if _, replaceErr := credCRUD.Replace(ctx, cred, nil); replaceErr != nil {
			return utils.TrackError(replaceErr)
		}
		logger.Info("marked credential as Revoked", "credential", cred.CosmosMetadata.ResourceID.Name)
	}
	if err := iter.GetError(); err != nil {
		return utils.TrackError(fmt.Errorf("iterating SystemAdminCredentials: %w", err))
	}

	// TODO(follow-up): per-credential CSR/CSRA/RBAC + CRR DeleteDesire teardown.
	// Each credential's OutstandingDesires list should be walked, the backing
	// ApplyDesires deleted, and corresponding DeleteDesires created to remove
	// the MC-side objects. This is omitted in the initial implementation and
	// will be addressed in a subsequent PR.

	// Clear the cluster's RevokeCredentialsOperationID.
	dbClient := c.resourcesDBClient.HCPClusters(
		oldOperation.ExternalID.SubscriptionID,
		oldOperation.ExternalID.ResourceGroupName,
	)
	cluster, err := dbClient.Get(ctx, oldOperation.ExternalID.Name)
	if err != nil {
		return utils.TrackError(err)
	}
	if cluster.ServiceProviderProperties.RevokeCredentialsOperationID == oldOperation.OperationID.Name {
		logger.Info("clearing RevokeCredentialsOperationID from cluster")
		cluster.ServiceProviderProperties.RevokeCredentialsOperationID = ""
		_, err = dbClient.Replace(ctx, cluster, nil)
		if err != nil {
			return utils.TrackError(err)
		}
	}

	// Complete the operation.
	newOperationStatus := arm.ProvisioningStateSucceeded
	if !needToPatchOperation(oldOperation, newOperationStatus, nil) {
		return nil
	}
	err = patchOperation(ctx, c.clock, c.resourcesDBClient, oldOperation, newOperationStatus, nil, postAsyncNotificationFn(c.notificationClient))
	if err != nil {
		return utils.TrackError(err)
	}

	return nil
}

// isCRRComplete reads the CRR ReadDesire mirror and returns true when the
// PreviousCertificatesRevoked condition is True. If the ReadDesire does not
// exist yet (e.g. kube-applier hasn't synced), returns false.
func (c *operationRevokeSystemAdminCredentials) isCRRComplete(
	ctx context.Context,
	operation *api.Operation,
) (bool, error) {
	spc, err := database.GetOrCreateServiceProviderCluster(ctx, c.resourcesDBClient, operation.ExternalID)
	if err != nil {
		return false, fmt.Errorf("get ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return false, nil // not placed yet — can't check
	}

	revokeOpSuffix := systemadmincredential.RevokeOpSuffix(operation.OperationID.Name)
	crrReadDesireName := fmt.Sprintf("system-admin-credential-revocation-%s", revokeOpSuffix)

	kaClient := c.kubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return false, nil // MC not in registry yet
	}
	readCRUD, err := kaClient.ReadDesiresForCluster(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
		operation.ExternalID.Name,
	)
	if err != nil {
		return false, fmt.Errorf("get ReadDesire CRUD: %w", err)
	}

	readDesire, err := readCRUD.Get(ctx, crrReadDesireName)
	if database.IsNotFoundError(err) {
		// TODO(follow-up): create the CRR ReadDesire alongside the ApplyDesire
		// in the dispatch controller. For now, log and return not-complete.
		utils.LoggerFromContext(ctx).Info("CRR ReadDesire not found yet", "name", crrReadDesireName)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get CRR ReadDesire: %w", err)
	}

	// Parse the mirrored CRR from the ReadDesire's status.KubeContent.
	if readDesire.Status.KubeContent == nil || len(readDesire.Status.KubeContent.Raw) == 0 {
		return false, nil // not observed yet
	}

	var crr hypershiftcertv1alpha1.CertificateRevocationRequest
	if err := json.Unmarshal(readDesire.Status.KubeContent.Raw, &crr); err != nil {
		return false, fmt.Errorf("unmarshaling CRR from ReadDesire: %w", err)
	}

	for _, cond := range crr.Status.Conditions {
		if cond.Type == hypershiftcertv1alpha1.PreviousCertificatesRevokedType &&
			cond.Status == metav1.ConditionTrue {
			return true, nil
		}
	}

	return false, nil
}

// Compile-time check: ensure we implement OperationSynchronizer.
var _ OperationSynchronizer = (*operationRevokeSystemAdminCredentials)(nil)
