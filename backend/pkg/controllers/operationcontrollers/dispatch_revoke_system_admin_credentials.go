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
	"time"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	utilsclock "k8s.io/utils/clock"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/systemadmincredential"
	"github.com/Azure/ARO-HCP/internal/utils"
	"github.com/Azure/ARO-HCP/internal/utils/apihelpers"
)

type dispatchRevokeSystemAdminCredentials struct {
	clock                utilsclock.PassiveClock
	resourcesDBClient    database.ResourcesDBClient
	kubeApplierDBClients database.KubeApplierDBClients

	// hostedClusterNamespaceEnvIdentifier is the "envName" segment of the
	// CDNamespace (ocm-<envName>-<csClusterID>).
	hostedClusterNamespaceEnvIdentifier string
}

// NewDispatchRevokeSystemAdminCredentialsController returns a Controller that
// marks every non-terminal SystemAdminCredential as AwaitingRevocation and
// writes a CRR ApplyDesire to the management cluster's kube-applier container.
//
// Operation documents relevant to this controller will have the following values:
//
//	ResourceType: Microsoft.RedHatOpenShift/hcpOpenShiftClusters
//	     Request: RevokeCredentials
//	      Status: Accepted
func NewDispatchRevokeSystemAdminCredentialsController(
	clock utilsclock.PassiveClock,
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
	activeOperationInformer cache.SharedIndexInformer,
) controllerutils.Controller {
	syncer := &dispatchRevokeSystemAdminCredentials{
		clock:                               clock,
		resourcesDBClient:                   resourcesDBClient,
		kubeApplierDBClients:                kubeApplierDBClients,
		hostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}

	controller := NewGenericOperationController(
		"DispatchRevokeSystemAdminCredentials",
		syncer,
		10*time.Second,
		activeOperationInformer,
		resourcesDBClient,
	)

	return controller
}

func (c *dispatchRevokeSystemAdminCredentials) ShouldProcess(ctx context.Context, operation *api.Operation) bool {
	if operation.Status.IsTerminal() {
		return false
	}
	if operation.Request != database.OperationRequestRevokeCredentials {
		return false
	}
	if operation.Status != arm.ProvisioningStateAccepted {
		return false
	}
	return true
}

func (c *dispatchRevokeSystemAdminCredentials) SynchronizeOperation(ctx context.Context, key controllerutils.OperationKey) error {
	logger := utils.LoggerFromContext(ctx)
	logger.Info("checking operation")

	operation, err := c.resourcesDBClient.Operations(key.SubscriptionID).Get(ctx, key.OperationName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get active operation: %w", err)
	}
	if !c.ShouldProcess(ctx, operation) {
		return nil
	}

	// Verify the cluster's RevokeCredentialsOperationID still matches.
	cluster, err := c.resourcesDBClient.HCPClusters(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
	).Get(ctx, operation.ExternalID.Name)
	if err != nil {
		return utils.TrackError(err)
	}
	if cluster.ServiceProviderProperties.RevokeCredentialsOperationID != operation.OperationID.Name {
		logger.Info("cluster RevokeCredentialsOperationID mismatch",
			"revoke_credentials_operation_id", cluster.ServiceProviderProperties.RevokeCredentialsOperationID)

		apihelpers.CancelOperation(operation, c.clock.Now())
		_, err = c.resourcesDBClient.Operations(key.SubscriptionID).Replace(ctx, operation, nil)
		if err != nil {
			return utils.TrackError(err)
		}
		return nil
	}

	// Mark every non-terminal credential as AwaitingRevocation.
	credCRUD := c.resourcesDBClient.HCPClusters(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
	).SystemAdminCredentials(operation.ExternalID.Name)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return utils.TrackError(err)
	}
	for _, cred := range iter.Items(ctx) {
		switch cred.Status.Phase {
		case api.SystemAdminCredentialPhaseRequested,
			api.SystemAdminCredentialPhaseIssued:
			cred.Status.Phase = api.SystemAdminCredentialPhaseAwaitingRevocation
			if _, replaceErr := credCRUD.Replace(ctx, cred, nil); replaceErr != nil {
				return utils.TrackError(replaceErr)
			}
			logger.Info("marked credential as AwaitingRevocation",
				"credential", cred.CosmosMetadata.ResourceID.Name)
		default:
			// Already revoked, failed, or awaiting — skip.
		}
	}
	if err := iter.GetError(); err != nil {
		return utils.TrackError(fmt.Errorf("iterating SystemAdminCredentials: %w", err))
	}

	// Write a CRR ApplyDesire to the kube-applier container so the MC
	// receives the CertificateRevocationRequest.
	if err := c.writeCRRDesire(ctx, operation, cluster); err != nil {
		return utils.TrackError(err)
	}

	// Advance operation to Deleting so the poll controller takes over.
	operation.Status = arm.ProvisioningStateDeleting
	_, err = c.resourcesDBClient.Operations(key.SubscriptionID).Replace(ctx, operation, nil)
	if err != nil {
		return utils.TrackError(err)
	}

	return nil
}

// writeCRRDesire creates an ApplyDesire containing the HyperShift
// CertificateRevocationRequest for the customer-break-glass signer class.
func (c *dispatchRevokeSystemAdminCredentials) writeCRRDesire(
	ctx context.Context,
	operation *api.Operation,
	cluster *api.HCPOpenShiftCluster,
) error {
	spc, err := database.GetOrCreateServiceProviderCluster(ctx, c.resourcesDBClient, operation.ExternalID)
	if err != nil {
		return fmt.Errorf("get ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return fmt.Errorf("management cluster not yet placed for cluster %s", operation.ExternalID.String())
	}
	if cluster.ServiceProviderProperties.ClusterServiceID == nil {
		return fmt.Errorf("cluster has no ClusterServiceID")
	}

	csClusterID := cluster.ServiceProviderProperties.ClusterServiceID.ID()
	hcpNamespace := fmt.Sprintf("ocm-%s-%s", c.hostedClusterNamespaceEnvIdentifier, csClusterID)
	revokeOpSuffix := systemadmincredential.RevokeOpSuffix(operation.OperationID.Name)

	clusterResourceID := operation.ExternalID

	crr := systemadmincredential.BuildRevocationRequest(clusterResourceID, revokeOpSuffix, hcpNamespace)

	crrJSON, err := json.Marshal(crr)
	if err != nil {
		return fmt.Errorf("marshaling CRR: %w", err)
	}

	desireName := fmt.Sprintf("system-admin-credential-revocation-%s", revokeOpSuffix)
	desireResourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
		operation.ExternalID.Name,
		desireName,
	)
	desireResourceID, err := azcorearm.ParseResourceID(desireResourceIDStr)
	if err != nil {
		return fmt.Errorf("parsing desire resource ID: %w", err)
	}

	desire := &kubeapplier.ApplyDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: desireResourceID},
		Spec: kubeapplier.ApplyDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:     "certificates.hypershift.openshift.io",
				Version:   "v1alpha1",
				Resource:  "certificaterevocationrequests",
				Namespace: hcpNamespace,
				Name:      crr.Name,
			},
			KubeContent: &runtime.RawExtension{Raw: crrJSON},
		},
	}

	kaClient := c.kubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return fmt.Errorf("kube-applier client not available for MC %s", mcResourceID.String())
	}
	crud, err := kaClient.ApplyDesiresForCluster(
		operation.ExternalID.SubscriptionID,
		operation.ExternalID.ResourceGroupName,
		operation.ExternalID.Name,
	)
	if err != nil {
		return fmt.Errorf("get ApplyDesire CRUD: %w", err)
	}

	// Idempotent: if the desire already exists, replace it.
	existing, getErr := crud.Get(ctx, desireName)
	if database.IsNotFoundError(getErr) {
		_, err = crud.Create(ctx, desire, nil)
	} else if getErr != nil {
		return fmt.Errorf("get existing CRR desire: %w", getErr)
	} else {
		desire.CosmosMetadata = *existing.CosmosMetadata.DeepCopy()
		_, err = crud.Replace(ctx, desire, nil)
	}
	if err != nil {
		return fmt.Errorf("write CRR ApplyDesire: %w", err)
	}

	return nil
}

// operationRevokeSystemAdminCredentials is defined in
// operation_revoke_system_admin_credentials.go.

// Compile-time check: ensure we implement OperationSynchronizer.
var (
	_ OperationSynchronizer = (*dispatchRevokeSystemAdminCredentials)(nil)
	_ metav1.Object         // side-effect import anchor
)
