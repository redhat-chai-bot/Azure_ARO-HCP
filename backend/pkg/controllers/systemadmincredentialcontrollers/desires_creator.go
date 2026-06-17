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
	"strings"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/systemadmincredential"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// desireToCreate describes a single ApplyDesire or ReadDesire to be created
// for a SystemAdminCredential.
type desireToCreate struct {
	// name is the desire document's name (last segment of its Cosmos resource ID).
	name string
	// kind selects ApplyDesire or ReadDesire.
	kind api.SystemAdminCredentialDesireKind
	// targetItem identifies the k8s object on the management cluster.
	targetItem kubeapplier.ResourceReference
	// kubeJSON is the JSON-serialized k8s object for ApplyDesire.Spec.KubeContent.
	// Nil for ReadDesires (the kube-applier fills status from watching).
	kubeJSON []byte
}

// DesiresCreator creates ApplyDesires and ReadDesires for SystemAdminCredentials
// in Phase=Requested. It is designed to be called periodically per cluster from
// a ClusterWatchingController or equivalent sweep.
//
// For each credential in Requested phase, it ensures the following desires exist
// in the kube-applier container for the cluster's management cluster:
//   - 8 ApplyDesires: CSR, CSRA, and 6 RBAC objects (2 per bundle × 3 bundles)
//   - 1 ReadDesire: the CSR (to observe status.certificate for issuance)
//
// Each desire it creates is tracked in the credential's Status.OutstandingDesires.
// Re-runs skip desires whose refs are already present, and ConflictError from a
// Create is treated as success.
type DesiresCreator struct {
	ResourcesDBClient                   database.ResourcesDBClient
	KubeApplierDBClients                database.KubeApplierDBClients
	HostedClusterNamespaceEnvIdentifier string
}

// NewDesiresCreator constructs a DesiresCreator.
func NewDesiresCreator(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	hostedClusterNamespaceEnvIdentifier string,
) *DesiresCreator {
	return &DesiresCreator{
		ResourcesDBClient:                   resourcesDBClient,
		KubeApplierDBClients:                kubeApplierDBClients,
		HostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}
}

// SyncCluster ensures desires exist for every SystemAdminCredential in
// Phase=Requested under the given cluster. It returns the number of
// credentials processed and the first error encountered (after attempting
// all eligible credentials).
func (dc *DesiresCreator) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := dc.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
	if database.IsNotFoundError(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("getting cluster: %w", err)
	}
	if cluster.ServiceProviderProperties.ClusterServiceID == nil {
		// No CS reference yet; retrigger once set.
		return 0, nil
	}

	// Build the cluster's ARM resource ID for the owner annotation and
	// for ServiceProviderCluster lookup.
	clusterResourceID, err := azcorearm.ParseResourceID(
		fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s",
			subscriptionID, resourceGroupName,
			api.ClusterResourceType.String(), clusterName))
	if err != nil {
		return 0, fmt.Errorf("parsing cluster resource ID: %w", err)
	}

	// Get the ServiceProviderCluster to determine MC placement.
	spc, err := database.GetOrCreateServiceProviderCluster(ctx, dc.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		// MC placement not resolved yet; retry on next reconcile.
		return 0, nil
	}

	kaClient := dc.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		// MC not registered in the kube-applier registry yet.
		return 0, nil
	}

	hcpNamespace := fmt.Sprintf("ocm-%s-%s",
		dc.HostedClusterNamespaceEnvIdentifier,
		cluster.ServiceProviderProperties.ClusterServiceID.ID())

	// --- List credentials and process Requested ones ---

	credCRUD := dc.ResourcesDBClient.HCPClusters(
		subscriptionID, resourceGroupName,
	).SystemAdminCredentials(clusterName)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("listing SystemAdminCredentials: %w", err)
	}

	processed := 0
	var firstErr error
	for _, cred := range iter.Items(ctx) {
		if cred.Status.Phase != api.SystemAdminCredentialPhaseRequested {
			continue
		}

		credName := cred.CosmosMetadata.ResourceID.Name
		if err := dc.createDesiresForCredential(
			ctx, credCRUD, cred, kaClient,
			clusterResourceID, mcResourceID, hcpNamespace,
		); err != nil {
			logger.Error(err, "failed to create desires for credential",
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

// createDesiresForCredential creates all ApplyDesires and ReadDesires for a
// single credential, and updates the credential's OutstandingDesires.
func (dc *DesiresCreator) createDesiresForCredential(
	ctx context.Context,
	credCRUD database.ResourceCRUD[api.SystemAdminCredential],
	cred *api.SystemAdminCredential,
	kaClient database.KubeApplierDBClient,
	clusterResourceID, mcResourceID *azcorearm.ResourceID,
	hcpNamespace string,
) error {
	logger := utils.LoggerFromContext(ctx)
	credName := cred.CosmosMetadata.ResourceID.Name
	subscriptionID := clusterResourceID.SubscriptionID
	resourceGroupName := clusterResourceID.ResourceGroupName
	clusterName := clusterResourceID.Name

	// Build all desire specs for this credential.
	specs, err := buildDesireSpecs(clusterResourceID, cred, hcpNamespace)
	if err != nil {
		return fmt.Errorf("building desire specs for credential %s: %w", credName, err)
	}

	// Get CRUDs for each desire kind (scoped to the cluster).
	applyCRUD, err := kaClient.ApplyDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return fmt.Errorf("getting ApplyDesire CRUD: %w", err)
	}
	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}

	// Track whether we need to update the credential doc.
	needsUpdate := false

	for _, spec := range specs {
		ref := api.SystemAdminCredentialDesireRef{Kind: spec.kind, Name: spec.name}

		// Skip if already tracked in OutstandingDesires.
		if hasDesireRef(cred.Status.OutstandingDesires, ref) {
			continue
		}

		// Ensure the desire exists in Cosmos.
		switch spec.kind {
		case api.SystemAdminCredentialDesireKindApply:
			if err := ensureApplyDesire(ctx, applyCRUD, spec,
				subscriptionID, resourceGroupName, clusterName, mcResourceID); err != nil {
				return fmt.Errorf("ensuring ApplyDesire %s: %w", spec.name, err)
			}
		case api.SystemAdminCredentialDesireKindRead:
			if err := ensureReadDesire(ctx, readCRUD, spec,
				subscriptionID, resourceGroupName, clusterName, mcResourceID); err != nil {
				return fmt.Errorf("ensuring ReadDesire %s: %w", spec.name, err)
			}
		}

		cred.Status.OutstandingDesires = append(cred.Status.OutstandingDesires, ref)
		needsUpdate = true
	}

	if needsUpdate {
		if _, err := credCRUD.Replace(ctx, cred, nil); err != nil {
			return fmt.Errorf("updating credential OutstandingDesires: %w", err)
		}
		logger.Info("updated OutstandingDesires for credential",
			"credential_name", credName,
			"desire_count", len(cred.Status.OutstandingDesires))
	}

	return nil
}

// ensureApplyDesire creates an ApplyDesire if it doesn't already exist.
// ConflictError from a concurrent Create is treated as success.
func ensureApplyDesire(
	ctx context.Context,
	crud database.ResourceCRUD[kubeapplier.ApplyDesire],
	spec desireToCreate,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	// Check if already exists.
	existing, err := crud.Get(ctx, spec.name)
	if err != nil && !database.IsNotFoundError(err) {
		return fmt.Errorf("checking existing: %w", err)
	}
	if existing != nil {
		return nil // already exists
	}

	resourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, spec.name)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return fmt.Errorf("parsing desire resource ID: %w", err)
	}

	desire := &kubeapplier.ApplyDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ApplyDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem:        spec.targetItem,
			KubeContent:       &runtime.RawExtension{Raw: spec.kubeJSON},
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

// ensureReadDesire creates a ReadDesire if it doesn't already exist.
// ConflictError from a concurrent Create is treated as success.
func ensureReadDesire(
	ctx context.Context,
	crud database.ResourceCRUD[kubeapplier.ReadDesire],
	spec desireToCreate,
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
) error {
	// Check if already exists.
	existing, err := crud.Get(ctx, spec.name)
	if err != nil && !database.IsNotFoundError(err) {
		return fmt.Errorf("checking existing: %w", err)
	}
	if existing != nil {
		return nil // already exists
	}

	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, spec.name)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return fmt.Errorf("parsing desire resource ID: %w", err)
	}

	desire := &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem:        spec.targetItem,
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

// buildDesireSpecs constructs the full list of ApplyDesire and ReadDesire
// specs for a single credential. Returns 8 ApplyDesires (CSR, CSRA, 6 RBAC
// objects) and 1 ReadDesire (CSR).
func buildDesireSpecs(
	clusterResourceID *azcorearm.ResourceID,
	cred *api.SystemAdminCredential,
	hcpNamespace string,
) ([]desireToCreate, error) {
	credName := cred.CosmosMetadata.ResourceID.Name

	// --- Build k8s objects ---

	csr, err := systemadmincredential.BuildCSR(
		clusterResourceID, credName, cred.Spec.Username,
		[]byte(cred.Spec.PrivateKeyPEM), hcpNamespace)
	if err != nil {
		return nil, fmt.Errorf("building CSR: %w", err)
	}
	csrJSON, err := json.Marshal(csr)
	if err != nil {
		return nil, fmt.Errorf("marshaling CSR: %w", err)
	}

	csra := systemadmincredential.BuildCSRA(clusterResourceID, credName, hcpNamespace)
	csraJSON, err := json.Marshal(csra)
	if err != nil {
		return nil, fmt.Errorf("marshaling CSRA: %w", err)
	}

	// --- Assemble desire specs ---

	csrDesireName := fmt.Sprintf("system-admin-credential-csr-%s", credName)

	specs := []desireToCreate{
		// CSR ApplyDesire
		{
			name:       csrDesireName,
			kind:       api.SystemAdminCredentialDesireKindApply,
			targetItem: resourceRefForCSR(csr.Name),
			kubeJSON:   csrJSON,
		},
		// CSRA ApplyDesire
		{
			name:       fmt.Sprintf("system-admin-credential-csra-%s", credName),
			kind:       api.SystemAdminCredentialDesireKindApply,
			targetItem: resourceRefForCSRA(csra.Name, hcpNamespace),
			kubeJSON:   csraJSON,
		},
		// CSR ReadDesire — same targetItem as the CSR ApplyDesire.
		// ApplyDesires and ReadDesires live in separate Cosmos containers,
		// so the names don't collide.
		{
			name:       csrDesireName,
			kind:       api.SystemAdminCredentialDesireKindRead,
			targetItem: resourceRefForCSR(csr.Name),
		},
	}

	// RBAC bundles: each helper returns []client.Object (a Role/ClusterRole +
	// a RoleBinding/ClusterRoleBinding). Each object gets its own ApplyDesire.
	rbacBundles := []struct {
		prefix string
		objs   []client.Object
	}{
		{
			prefix: "system-admin-credential-give-csr-perm",
			objs:   systemadmincredential.BuildRBACGiveCSRPerm(clusterResourceID, credName),
		},
		{
			prefix: "system-admin-credential-csra-perm",
			objs:   systemadmincredential.BuildRBACCSRA(clusterResourceID, credName, hcpNamespace),
		},
		{
			prefix: "system-admin-credential-revocation-perm",
			objs:   systemadmincredential.BuildRBACRevocation(clusterResourceID, credName, hcpNamespace),
		},
	}

	for _, bundle := range rbacBundles {
		for _, obj := range bundle.objs {
			objJSON, err := json.Marshal(obj)
			if err != nil {
				return nil, fmt.Errorf("marshaling %s %s: %w",
					bundle.prefix, obj.GetObjectKind().GroupVersionKind().Kind, err)
			}
			kindSuffix := rbacKindSuffix(obj.GetObjectKind().GroupVersionKind().Kind)
			specs = append(specs, desireToCreate{
				name:       fmt.Sprintf("%s-%s-%s", bundle.prefix, kindSuffix, credName),
				kind:       api.SystemAdminCredentialDesireKindApply,
				targetItem: resourceRefForRBACObject(obj),
				kubeJSON:   objJSON,
			})
		}
	}

	return specs, nil
}

// resourceRefForCSR builds the ResourceReference for a CertificateSigningRequest.
func resourceRefForCSR(name string) kubeapplier.ResourceReference {
	return kubeapplier.ResourceReference{
		Group:    "certificates.k8s.io",
		Version:  "v1",
		Resource: "certificatesigningrequests",
		Name:     name,
	}
}

// resourceRefForCSRA builds the ResourceReference for a CertificateSigningRequestApproval.
func resourceRefForCSRA(name, namespace string) kubeapplier.ResourceReference {
	return kubeapplier.ResourceReference{
		Group:     "certificates.hypershift.openshift.io",
		Version:   "v1alpha1",
		Resource:  "certificatesigningrequestapprovals",
		Namespace: namespace,
		Name:      name,
	}
}

// resourceRefForRBACObject builds the ResourceReference for an RBAC k8s object.
// The object's TypeMeta must be populated (the Build* helpers always set it).
func resourceRefForRBACObject(obj client.Object) kubeapplier.ResourceReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return kubeapplier.ResourceReference{
		Group:     gvk.Group,
		Version:   gvk.Version,
		Resource:  strings.ToLower(gvk.Kind) + "s",
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// rbacKindSuffix returns a short suffix for desire naming based on the k8s
// Kind. Used to produce unique desire names when a bundle contains multiple objects.
func rbacKindSuffix(kind string) string {
	switch kind {
	case "ClusterRole":
		return "cr"
	case "ClusterRoleBinding":
		return "crb"
	case "Role":
		return "role"
	case "RoleBinding":
		return "rb"
	default:
		return strings.ToLower(kind)
	}
}

// hasDesireRef reports whether the given ref is already present in the list.
func hasDesireRef(refs []api.SystemAdminCredentialDesireRef, target api.SystemAdminCredentialDesireRef) bool {
	for _, r := range refs {
		if r.Kind == target.Kind && r.Name == target.Name {
			return true
		}
	}
	return false
}
