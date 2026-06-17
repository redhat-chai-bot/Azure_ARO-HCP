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
	"encoding/base64"
	"encoding/json"
	"fmt"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	certificatesv1 "k8s.io/api/certificates/v1"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// IssuanceObserver watches ReadDesire status for CSR objects. When the
// kube-applier has mirrored a CertificateSigningRequest whose
// status.certificate is populated (meaning the MC signer has issued the
// cert), it transitions the corresponding SystemAdminCredential from
// Phase=Requested to Phase=Issued and stores the signed certificate.
//
// It is designed to be called periodically per cluster from a
// ClusterWatchingController or equivalent sweep.
type IssuanceObserver struct {
	ResourcesDBClient    database.ResourcesDBClient
	KubeApplierDBClients database.KubeApplierDBClients
}

// NewIssuanceObserver constructs an IssuanceObserver.
func NewIssuanceObserver(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
) *IssuanceObserver {
	return &IssuanceObserver{
		ResourcesDBClient:    resourcesDBClient,
		KubeApplierDBClients: kubeApplierDBClients,
	}
}

// SyncCluster checks all SystemAdminCredentials in Phase=Requested under the
// given cluster. For each credential whose CSR ReadDesire has a signed
// certificate in its mirrored content, it transitions the credential to
// Phase=Issued. It returns the number of credentials transitioned and the
// first error encountered (after attempting all eligible credentials).
func (o *IssuanceObserver) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	// --- Resolve cluster prerequisites ---

	cluster, err := o.ResourcesDBClient.HCPClusters(subscriptionID, resourceGroupName).Get(ctx, clusterName)
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

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, o.ResourcesDBClient, clusterResourceID)
	if err != nil {
		return 0, fmt.Errorf("getting ServiceProviderCluster: %w", err)
	}
	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return 0, nil
	}

	kaClient := o.KubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return 0, nil
	}

	// --- List credentials and check for issuance ---

	credCRUD := o.ResourcesDBClient.HCPClusters(
		subscriptionID, resourceGroupName,
	).SystemAdminCredentials(clusterName)

	readCRUD, err := kaClient.ReadDesiresForCluster(subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return 0, fmt.Errorf("getting ReadDesire CRUD: %w", err)
	}

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

		// Find the CSR ReadDesire ref in OutstandingDesires.
		csrReadDesireName := findCSRReadDesireName(cred.Status.OutstandingDesires)
		if csrReadDesireName == "" {
			// No CSR ReadDesire tracked yet; DesiresCreator hasn't run
			// or hasn't created the read desire for this credential.
			continue
		}

		cert, err := extractSignedCertificate(ctx, readCRUD, csrReadDesireName)
		if err != nil {
			logger.Error(err, "failed to check CSR ReadDesire for credential",
				"credential_name", credName,
				"read_desire_name", csrReadDesireName)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if cert == "" {
			// Certificate not yet issued; will check again on next sync.
			continue
		}

		// Transition to Issued.
		cred.Status.Phase = api.SystemAdminCredentialPhaseIssued
		cred.Status.SignedCertificate = cert

		if _, err := credCRUD.Replace(ctx, cred, nil); err != nil {
			logger.Error(err, "failed to update credential to Issued",
				"credential_name", credName)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		logger.Info("credential transitioned to Issued",
			"credential_name", credName)
		processed++
	}
	if iterErr := iter.GetError(); iterErr != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("iterating SystemAdminCredentials: %w", iterErr)
		}
	}

	return processed, firstErr
}

// findCSRReadDesireName returns the name of the CSR ReadDesire from the
// credential's OutstandingDesires list, or "" if none is found.
// The CSR ReadDesire is identified by Kind=ReadDesire and a name prefix
// of "system-admin-credential-csr-".
func findCSRReadDesireName(refs []api.SystemAdminCredentialDesireRef) string {
	for _, ref := range refs {
		if ref.Kind != api.SystemAdminCredentialDesireKindRead {
			continue
		}
		// CSR ReadDesire names follow the pattern set by DesiresCreator:
		// "system-admin-credential-csr-<credName>"
		if len(ref.Name) > len("system-admin-credential-csr-") &&
			ref.Name[:len("system-admin-credential-csr-")] == "system-admin-credential-csr-" {
			return ref.Name
		}
	}
	return ""
}

// extractSignedCertificate reads the CSR ReadDesire and extracts the signed
// certificate from its mirrored KubeContent. Returns the base64-encoded
// certificate, or "" if the certificate has not been issued yet.
func extractSignedCertificate(
	ctx context.Context,
	readCRUD database.ResourceCRUD[kubeapplier.ReadDesire],
	readDesireName string,
) (string, error) {
	desire, err := readCRUD.Get(ctx, readDesireName)
	if err != nil {
		if database.IsNotFoundError(err) {
			// ReadDesire doesn't exist yet; DesiresCreator may not have
			// persisted it or it was cleaned up.
			return "", nil
		}
		return "", fmt.Errorf("getting ReadDesire %s: %w", readDesireName, err)
	}

	if desire.Status.KubeContent == nil || desire.Status.KubeContent.Raw == nil {
		// The kube-applier hasn't mirrored the CSR yet.
		return "", nil
	}

	// Unmarshal the mirrored CertificateSigningRequest.
	var csr certificatesv1.CertificateSigningRequest
	if err := json.Unmarshal(desire.Status.KubeContent.Raw, &csr); err != nil {
		return "", fmt.Errorf("unmarshaling mirrored CSR from ReadDesire %s: %w", readDesireName, err)
	}

	if len(csr.Status.Certificate) == 0 {
		// CSR exists on the MC but has not been signed yet.
		return "", nil
	}

	// The CSR status.certificate is raw DER bytes; encode to base64 for
	// storage in the credential document.
	return base64.StdEncoding.EncodeToString(csr.Status.Certificate), nil
}
