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

package systemadmincredentialcontrollers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// newTestReadDesireWithCert creates a ReadDesire whose Status.KubeContent
// contains a CertificateSigningRequest with the given certificate bytes.
// If cert is nil, the CSR has no certificate yet.
func newTestReadDesireWithCert(credName string, cert []byte) *kubeapplier.ReadDesire {
	desireName := fmt.Sprintf("system-admin-credential-csr-%s", credName)

	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		testSubscriptionID, testResourceGroupName, testClusterName, desireName)
	resourceID := api.Must(azcorearm.ParseResourceID(resourceIDStr))

	mcResourceID := newTestManagementClusterResourceID()

	desire := &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:    "certificates.k8s.io",
				Version:  "v1",
				Resource: "certificatesigningrequests",
				Name:     fmt.Sprintf("system-admin-credential-csr-%s", credName),
			},
		},
	}

	// Build a CertificateSigningRequest to mirror in KubeContent.
	csr := &certificatesv1.CertificateSigningRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "certificates.k8s.io/v1",
			Kind:       "CertificateSigningRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: desireName,
		},
	}
	if len(cert) > 0 {
		csr.Status.Certificate = cert
	}

	csrJSON, err := json.Marshal(csr)
	if err != nil {
		panic(err)
	}
	desire.Status.KubeContent = &runtime.RawExtension{Raw: csrJSON}

	return desire
}

// newTestReadDesireWithoutContent creates a ReadDesire with nil KubeContent
// (kube-applier hasn't mirrored anything yet).
func newTestReadDesireWithoutContent(credName string) *kubeapplier.ReadDesire {
	desireName := fmt.Sprintf("system-admin-credential-csr-%s", credName)

	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		testSubscriptionID, testResourceGroupName, testClusterName, desireName)
	resourceID := api.Must(azcorearm.ParseResourceID(resourceIDStr))

	mcResourceID := newTestManagementClusterResourceID()

	return &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:    "certificates.k8s.io",
				Version:  "v1",
				Resource: "certificatesigningrequests",
				Name:     desireName,
			},
		},
	}
}

// newTestCredentialWithOutstandingDesires creates a credential in the given
// phase that already has a CSR ReadDesire ref in OutstandingDesires (as if
// DesiresCreator has already run).
func newTestCredentialWithOutstandingDesires(name string, phase api.SystemAdminCredentialPhase) *api.SystemAdminCredential {
	cred := newTestCredential(name, phase)
	cred.Status.OutstandingDesires = []api.SystemAdminCredentialDesireRef{
		{
			Kind: api.SystemAdminCredentialDesireKindRead,
			Name: fmt.Sprintf("system-admin-credential-csr-%s", name),
		},
	}
	return cred
}

func TestIssuanceObserver_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()
	fakeCert := []byte("-----BEGIN CERTIFICATE-----\nfake-signed-cert\n-----END CERTIFICATE-----\n")

	tests := []struct {
		name            string
		credentials     []*api.SystemAdminCredential
		readDesires     []any // seeded into kube-applier mock
		expectProcessed int
		expectError     bool
		expectPhase     api.SystemAdminCredentialPhase // expected phase after sync
		expectCert      bool                           // expect SignedCertificate to be set
	}{
		{
			name: "csr certificate present transitions to Issued",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithOutstandingDesires(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			readDesires: []any{
				newTestReadDesireWithCert(testCredentialName, fakeCert),
			},
			expectProcessed: 1,
			expectError:     false,
			expectPhase:     api.SystemAdminCredentialPhaseIssued,
			expectCert:      true,
		},
		{
			name: "csr certificate absent stays Requested",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithOutstandingDesires(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			readDesires: []any{
				newTestReadDesireWithCert(testCredentialName, nil),
			},
			expectProcessed: 0,
			expectError:     false,
			expectPhase:     api.SystemAdminCredentialPhaseRequested,
			expectCert:      false,
		},
		{
			name: "kube content not mirrored yet stays Requested",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithOutstandingDesires(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			readDesires: []any{
				newTestReadDesireWithoutContent(testCredentialName),
			},
			expectProcessed: 0,
			expectError:     false,
			expectPhase:     api.SystemAdminCredentialPhaseRequested,
			expectCert:      false,
		},
		{
			name: "no requested credentials is a no-op",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithOutstandingDesires(testCredentialName, api.SystemAdminCredentialPhaseIssued),
			},
			readDesires:     []any{},
			expectProcessed: 0,
			expectError:     false,
			expectPhase:     api.SystemAdminCredentialPhaseIssued,
			expectCert:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			// Seed the resources DB.
			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			})
			require.NoError(t, err)

			for _, cred := range tt.credentials {
				seedCredential(ctx, t, mockResourcesDBClient, cred)
			}

			// Set up the kube-applier mock with ReadDesires.
			mockKAClient, err := databasetesting.NewMockKubeApplierDBClientWithResources(ctx, tt.readDesires)
			require.NoError(t, err)
			mockKAClients := databasetesting.NewMockKubeApplierDBClients()
			mockKAClients.Register(mcResourceID, mockKAClient)

			// Construct and run the controller.
			observer := NewIssuanceObserver(mockResourcesDBClient, mockKAClients)
			processed, err := observer.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectProcessed, processed, "processed count mismatch")

			// Verify the credential's state.
			if len(tt.credentials) > 0 {
				credCRUD := mockResourcesDBClient.HCPClusters(
					testSubscriptionID, testResourceGroupName,
				).SystemAdminCredentials(testClusterName)
				updatedCred, err := credCRUD.Get(ctx, tt.credentials[0].CosmosMetadata.ResourceID.Name)
				require.NoError(t, err)
				assert.Equal(t, tt.expectPhase, updatedCred.Status.Phase,
					"credential phase mismatch")
				if tt.expectCert {
					assert.NotEmpty(t, updatedCred.Status.SignedCertificate,
						"credential should have SignedCertificate set")
				} else {
					assert.Empty(t, updatedCred.Status.SignedCertificate,
						"credential should not have SignedCertificate set")
				}
			}
		})
	}
}
