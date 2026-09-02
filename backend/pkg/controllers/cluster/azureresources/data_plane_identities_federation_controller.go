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

package azureresources

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"

	"github.com/Azure/ARO-HCP/internal/azure"

	azureclient "github.com/Azure/ARO-HCP/backend/pkg/azure/client"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	unionkubeapplierinformers "github.com/Azure/ARO-HCP/internal/database/unioninformers/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// DataPlaneIdentitiesFederationControllerName is the single source of truth for this
// controller's name. It is used for the workqueue name (a Prometheus label),
// context/logger controller name, and log fields.
const DataPlaneIdentitiesFederationControllerName = "DataPlaneIdentitiesFederation"

// ficAudience is the audience value used for federated identity credentials.
const ficAudience = "openshift"

// dataPlaneIdentitiesFederationSyncer manages federated identity credentials (FICs)
// for data plane operator managed identities, enabling OIDC workload identity
// federation so data plane operators can authenticate to Azure using Kubernetes
// service account tokens.
//
// It authenticates as the cluster's Service Managed Identity (via the SMI client
// builder) to manage FICs on the data plane operator managed identities.
type dataPlaneIdentitiesFederationSyncer struct {
	resourcesDBClient             corecosmosstorage.ResourcesDBClient
	clusterLister                 corelisters.ClusterLister
	serviceProviderClusterLister  corelisters.ServiceProviderClusterLister
	azureSMIClientBuilder         azureclient.ServiceManagedIdentityClientBuilder
	clusterScopedIdentitiesConfig *azure.ClusterScopedIdentitiesConfig
}

var _ controllerutils.ClusterSyncer = (*dataPlaneIdentitiesFederationSyncer)(nil)

// NewDataPlaneIdentitiesFederationController creates a cluster-watching controller that
// manages federated identity credentials for data plane operator managed identities.
func NewDataPlaneIdentitiesFederationController(
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister,
	azureSMIClientBuilder azureclient.ServiceManagedIdentityClientBuilder,
	clusterScopedIdentitiesConfig *azure.ClusterScopedIdentitiesConfig,
	informers coreinformers.BackendInformers,
	kubeApplierInformers *unionkubeapplierinformers.UnionKubeApplierInformers,
) controllerutils.Controller {
	_, clusterLister := informers.Clusters()

	syncer := &dataPlaneIdentitiesFederationSyncer{
		resourcesDBClient:             resourcesDBClient,
		clusterLister:                 clusterLister,
		serviceProviderClusterLister:  serviceProviderClusterLister,
		azureSMIClientBuilder:         azureSMIClientBuilder,
		clusterScopedIdentitiesConfig: clusterScopedIdentitiesConfig,
	}

	return controllerutils.NewClusterWatchingController(
		DataPlaneIdentitiesFederationControllerName,
		resourcesDBClient,
		informers,
		kubeApplierInformers,
		5*time.Minute,
		syncer,
	)
}

// NeedsWork reports whether SyncOnce has anything to do.
//
//   - While the cluster is being deleted there is work only while any FIC references
//     still exist: we delete FICs and clear tracking until done.
//   - While the cluster is not being deleted there is work while any FIC is
//     still pending or missing. Once all are confirmed, nothing more to do.
func (c *dataPlaneIdentitiesFederationSyncer) NeedsWork(cluster *coreapi.HCPOpenShiftCluster, serviceProviderCluster *coreapi.ServiceProviderCluster) bool {
	ficTracking := serviceProviderCluster.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials

	if cluster.ServiceProviderProperties.DeletionTimestamp != nil {
		// During deletion, we have work if any identities are still tracked.
		return len(ficTracking.Identities) > 0
	}

	// Build the desired set of (resourceID -> subjects) and check each is confirmed.
	dataPlaneOperators := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators
	if len(dataPlaneOperators) == 0 {
		return false
	}

	desiredEntries := c.buildDesiredFICEntries(dataPlaneOperators)
	if len(desiredEntries) == 0 {
		return false
	}

	for resourceIDKey, subjects := range desiredEntries {
		identityTracking, ok := ficTracking.Identities[resourceIDKey]
		if !ok {
			return true
		}
		for _, subject := range subjects {
			ref, ok := identityTracking.Credentials[subject.oidcSubject]
			if !ok || !ref.Confirmed {
				return true
			}
		}
	}

	return false
}

// ficSubjectEntry describes one FIC to create/validate for a given identity.
type ficSubjectEntry struct {
	operatorName string
	oidcSubject  string
	saNamespace  string
	saName       string
}

// buildDesiredFICEntries builds the desired set of FICs keyed by lowercased
// resourceID string. Multiple operators may share an identity.
func (c *dataPlaneIdentitiesFederationSyncer) buildDesiredFICEntries(dataPlaneOperators map[string]*azcorearm.ResourceID) map[string][]ficSubjectEntry {
	desired := make(map[string][]ficSubjectEntry)
	for operatorName, identityResourceID := range dataPlaneOperators {
		if identityResourceID == nil {
			continue
		}
		operatorConfig, ok := c.clusterScopedIdentitiesConfig.DataPlaneOperatorsIdentities[azure.ClusterOperatorIdentifier(operatorName)]
		if !ok {
			continue
		}
		resourceIDKey := strings.ToLower(identityResourceID.String())
		for _, sa := range operatorConfig.KubernetesServiceAccounts {
			desired[resourceIDKey] = append(desired[resourceIDKey], ficSubjectEntry{
				operatorName: operatorName,
				oidcSubject:  sa.AsOIDCSubject(),
				saNamespace:  sa.Namespace,
				saName:       sa.Name,
			})
		}
	}
	return desired
}

// SyncOnce reads the cluster and ServiceProviderCluster from the informer caches,
// short-circuits via NeedsWork, and then dispatches to the deletion or
// non-deletion (reconcile) path.
func (c *dataPlaneIdentitiesFederationSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	cluster, err := c.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster: %w", err))
	}

	existingServiceProviderCluster, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderCluster: %w", err))
	}

	if !c.NeedsWork(cluster, existingServiceProviderCluster) {
		return nil
	}

	if cluster.ServiceProviderProperties.DeletionTimestamp != nil {
		return c.deleteDataPlaneIdentitiesFederation(ctx, cluster, existingServiceProviderCluster)
	}
	return c.reconcileDataPlaneIdentitiesFederation(ctx, cluster, existingServiceProviderCluster)
}

// reconcileDataPlaneIdentitiesFederation iterates over all data plane operators and
// their service accounts, creating or validating FICs as needed.
func (c *dataPlaneIdentitiesFederationSyncer) reconcileDataPlaneIdentitiesFederation(
	ctx context.Context,
	cluster *coreapi.HCPOpenShiftCluster,
	existingServiceProviderCluster *coreapi.ServiceProviderCluster,
) error {
	logger := utils.LoggerFromContext(ctx)
	dataPlaneOperators := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators
	issuerURL := cluster.ServiceProviderProperties.Platform.IssuerURL

	if len(issuerURL) == 0 {
		return utils.TrackError(fmt.Errorf("issuer URL is empty for cluster %q", cluster.ID.String()))
	}

	smiResourceID := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.ServiceManagedIdentity
	if smiResourceID == nil {
		return utils.TrackError(fmt.Errorf("cluster ServiceManagedIdentity is nil; cannot manage FICs"))
	}

	clusterIdentityURL := cluster.ServiceProviderProperties.ManagedIdentitiesDataPlaneIdentityURL
	ficClient, err := c.azureSMIClientBuilder.FederatedIdentityCredentialsClient(ctx, clusterIdentityURL, smiResourceID, cluster.ID.SubscriptionID)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to build FIC client: %w", err))
	}

	replacement := existingServiceProviderCluster.DeepCopy()
	if replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials.Identities == nil {
		replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials.Identities = make(map[string]*coreapi.ManagedIdentityFederatedCredentials)
	}

	desiredEntries := c.buildDesiredFICEntries(dataPlaneOperators)

	for resourceIDKey, subjects := range desiredEntries {
		identityResourceID, err := azcorearm.ParseResourceID(resourceIDKey)
		if err != nil {
			return utils.TrackError(fmt.Errorf("failed to parse identity resource ID %q: %w", resourceIDKey, err))
		}

		if _, ok := replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials.Identities[resourceIDKey]; !ok {
			replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials.Identities[resourceIDKey] = &coreapi.ManagedIdentityFederatedCredentials{
				Credentials: make(map[string]*coreapi.FederatedIdentityCredentialReference),
			}
		}
		identityTracking := replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials.Identities[resourceIDKey]

		rgName := identityResourceID.ResourceGroupName
		miName := identityResourceID.Name

		for _, entry := range subjects {
			ficName := GenerateFICName(cluster.ID.Name, entry.operatorName, entry.saNamespace, entry.saName)

			// Check if already confirmed.
			if ref, ok := identityTracking.Credentials[entry.oidcSubject]; ok && ref.Confirmed {
				continue
			}

			// Mark as pending before checking Azure.
			identityTracking.Credentials[entry.oidcSubject] = &coreapi.FederatedIdentityCredentialReference{
				FICName: ficName,
				Pending: true,
			}

			getResp, getErr := ficClient.Get(ctx, rgName, miName, ficName, nil)
			if getErr != nil {
				var azErr *azcore.ResponseError
				if errors.As(getErr, &azErr) && azErr.StatusCode == 404 {
					// FIC does not exist yet, create it.
					logger.Info("creating federated identity credential",
						"operator", entry.operatorName, "ficName", ficName, "subject", entry.oidcSubject)

					_, createErr := ficClient.CreateOrUpdate(ctx, rgName, miName, ficName,
						armmsi.FederatedIdentityCredential{
							Properties: &armmsi.FederatedIdentityCredentialProperties{
								Issuer:    to.Ptr(issuerURL),
								Subject:   to.Ptr(entry.oidcSubject),
								Audiences: []*string{to.Ptr(ficAudience)},
							},
						}, nil)
					if createErr != nil {
						return utils.TrackError(fmt.Errorf("failed to create FIC %q for operator %q: %w", ficName, entry.operatorName, createErr))
					}

					identityTracking.Credentials[entry.oidcSubject] = &coreapi.FederatedIdentityCredentialReference{
						FICName:   ficName,
						Pending:   false,
						Confirmed: true,
					}
					continue
				}
				return utils.TrackError(fmt.Errorf("failed to get FIC %q for operator %q: %w", ficName, entry.operatorName, getErr))
			}

			// FIC exists, validate its properties.
			if c.validateFederatedIdentityCredential(getResp.FederatedIdentityCredential, issuerURL, entry.oidcSubject) {
				identityTracking.Credentials[entry.oidcSubject] = &coreapi.FederatedIdentityCredentialReference{
					FICName:   ficName,
					Pending:   false,
					Confirmed: true,
				}
			} else {
				// Properties mismatch, update the FIC.
				logger.Info("updating federated identity credential with corrected properties",
					"operator", entry.operatorName, "ficName", ficName, "subject", entry.oidcSubject)

				_, updateErr := ficClient.CreateOrUpdate(ctx, rgName, miName, ficName,
					armmsi.FederatedIdentityCredential{
						Properties: &armmsi.FederatedIdentityCredentialProperties{
							Issuer:    to.Ptr(issuerURL),
							Subject:   to.Ptr(entry.oidcSubject),
							Audiences: []*string{to.Ptr(ficAudience)},
						},
					}, nil)
				if updateErr != nil {
					return utils.TrackError(fmt.Errorf("failed to update FIC %q for operator %q: %w", ficName, entry.operatorName, updateErr))
				}

				identityTracking.Credentials[entry.oidcSubject] = &coreapi.FederatedIdentityCredentialReference{
					FICName:   ficName,
					Pending:   false,
					Confirmed: true,
				}
			}
		}
	}

	_, err = c.persistDataPlaneIdentitiesFederatedCredentials(ctx, cluster, existingServiceProviderCluster, replacement)
	return utils.TrackError(err)
}

// deleteDataPlaneIdentitiesFederation iterates over all tracked FICs and deletes them.
func (c *dataPlaneIdentitiesFederationSyncer) deleteDataPlaneIdentitiesFederation(
	ctx context.Context,
	cluster *coreapi.HCPOpenShiftCluster,
	existingServiceProviderCluster *coreapi.ServiceProviderCluster,
) error {
	logger := utils.LoggerFromContext(ctx)
	dataPlaneOperators := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators

	smiResourceID := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.ServiceManagedIdentity
	if smiResourceID == nil {
		// Cannot build the client; clear the tracking since the cluster is being deleted.
		return utils.TrackError(c.clearAllFICReferences(ctx, cluster, existingServiceProviderCluster))
	}

	clusterIdentityURL := cluster.ServiceProviderProperties.ManagedIdentitiesDataPlaneIdentityURL
	ficClient, err := c.azureSMIClientBuilder.FederatedIdentityCredentialsClient(ctx, clusterIdentityURL, smiResourceID, cluster.ID.SubscriptionID)
	if err != nil {
		// If we cannot build the client, clear the tracking anyway since the cluster is being deleted.
		return utils.TrackError(c.clearAllFICReferences(ctx, cluster, existingServiceProviderCluster))
	}

	var deleteErrors []error
	for operatorName, identityResourceID := range dataPlaneOperators {
		if identityResourceID == nil {
			continue
		}

		operatorConfig, ok := c.clusterScopedIdentitiesConfig.DataPlaneOperatorsIdentities[azure.ClusterOperatorIdentifier(operatorName)]
		if !ok {
			continue
		}

		rgName := identityResourceID.ResourceGroupName
		miName := identityResourceID.Name

		for _, sa := range operatorConfig.KubernetesServiceAccounts {
			ficName := GenerateFICName(cluster.ID.Name, operatorName, sa.Namespace, sa.Name)

			logger.Info("deleting federated identity credential",
				"operator", operatorName, "ficName", ficName)

			_, deleteErr := ficClient.Delete(ctx, rgName, miName, ficName, nil)
			if deleteErr != nil {
				if isDeleteNotFoundOrGone(deleteErr) {
					continue
				}
				deleteErrors = append(deleteErrors, fmt.Errorf("failed to delete FIC %q for operator %q: %w", ficName, operatorName, deleteErr))
			}
		}
	}

	if len(deleteErrors) > 0 {
		return utils.TrackError(errors.Join(deleteErrors...))
	}

	return utils.TrackError(c.clearAllFICReferences(ctx, cluster, existingServiceProviderCluster))
}

// validateFederatedIdentityCredential checks that the FIC's properties match
// the expected issuer URL, subject, and audience.
func (c *dataPlaneIdentitiesFederationSyncer) validateFederatedIdentityCredential(
	fic armmsi.FederatedIdentityCredential,
	expectedIssuerURL string,
	expectedSubject string,
) bool {
	if fic.Properties == nil {
		return false
	}

	if fic.Properties.Issuer == nil || *fic.Properties.Issuer != expectedIssuerURL {
		return false
	}

	if fic.Properties.Subject == nil || *fic.Properties.Subject != expectedSubject {
		return false
	}

	if len(fic.Properties.Audiences) != 1 || fic.Properties.Audiences[0] == nil || *fic.Properties.Audiences[0] != ficAudience {
		return false
	}

	return true
}

// persistDataPlaneIdentitiesFederatedCredentials replaces the ServiceProviderCluster
// when the FIC tracking state has changed.
func (c *dataPlaneIdentitiesFederationSyncer) persistDataPlaneIdentitiesFederatedCredentials(
	ctx context.Context,
	cluster *coreapi.HCPOpenShiftCluster,
	existing, replacement *coreapi.ServiceProviderCluster,
) (*coreapi.ServiceProviderCluster, error) {
	if !controllerutil.NeedsUpdate(existing, replacement) {
		return existing, nil
	}

	logger := utils.LoggerFromContext(ctx)
	logger.Info("persisting data plane identities federated credentials state onto ServiceProviderCluster")

	updated, err := c.resourcesDBClient.ServiceProviderClusters(cluster.ID.SubscriptionID, cluster.ID.ResourceGroupName, cluster.ID.Name).Replace(ctx, replacement, nil)
	if cosmosstorageutils.IsPreconditionFailedError(err) {
		return existing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to replace ServiceProviderCluster: %w", err)
	}
	return updated, nil
}

// clearAllFICReferences clears all FIC tracking from the ServiceProviderCluster.
func (c *dataPlaneIdentitiesFederationSyncer) clearAllFICReferences(
	ctx context.Context,
	cluster *coreapi.HCPOpenShiftCluster,
	existingServiceProviderCluster *coreapi.ServiceProviderCluster,
) error {
	replacement := existingServiceProviderCluster.DeepCopy()
	replacement.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials = coreapi.DataPlaneIdentitiesFederatedCredentials{}
	_, err := c.persistDataPlaneIdentitiesFederatedCredentials(ctx, cluster, existingServiceProviderCluster, replacement)
	return err
}

// isDeleteNotFoundOrGone reports whether err indicates the FIC, its parent MI,
// or the resource group no longer exists, or authorization has been removed.
// During cluster deletion all of these are expected and treated as success.
func isDeleteNotFoundOrGone(err error) bool {
	var azErr *azcore.ResponseError
	if !errors.As(err, &azErr) {
		return false
	}
	switch azErr.ErrorCode {
	case "ResourceNotFound", "ResourceGroupNotFound", "ParentResourceNotFound",
		"FederatedIdentityCredentialNotFound", "ManagedIdentityNotFound":
		return true
	}
	if azErr.StatusCode == 404 || azErr.StatusCode == 403 {
		return true
	}
	return false
}

// GenerateFICName generates a deterministic FIC name from the cluster name,
// operator name, service account namespace, and service account name.
// The name is based on a SHA-256 hash to stay within Azure's naming constraints
// (3-120 characters, alphanumeric and hyphens, must start with alphanumeric).
func GenerateFICName(clusterName string, operatorName string, saNamespace string, saName string) string {
	input := fmt.Sprintf("%s/%s/%s/%s", clusterName, operatorName, saNamespace, saName)
	hash := sha256.Sum256([]byte(input))
	// Use first 16 bytes (32 hex chars) for a compact deterministic name.
	hexHash := fmt.Sprintf("%x", hash[:16])

	// Sanitize the operator name for use as a prefix (alphanumeric and hyphens only).
	sanitized := sanitizeForAzureName(operatorName)

	// Format: "fic-<sanitized-operator>-<hash>" to stay within 120 chars
	// and start with a letter.
	name := fmt.Sprintf("fic-%s-%s", sanitized, hexHash)
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// sanitizeForAzureName replaces characters not allowed in Azure FIC names
// (only alphanumeric and hyphens are allowed) and ensures the result is
// not empty.
func sanitizeForAzureName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	result := b.String()
	// Trim leading/trailing hyphens.
	result = strings.Trim(result, "-")
	if len(result) == 0 {
		return "op"
	}
	return result
}
