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
	"fmt"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// newTestApplyDesire creates an ApplyDesire for the given desire name, suitable
// for seeding the kube-applier mock.
func newTestApplyDesire(desireName string) *kubeapplier.ApplyDesire {
	resourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
		testSubscriptionID, testResourceGroupName, testClusterName, desireName)
	resourceID := api.Must(azcorearm.ParseResourceID(resourceIDStr))
	mcResourceID := newTestManagementClusterResourceID()

	return &kubeapplier.ApplyDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ApplyDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:    "certificates.k8s.io",
				Version:  "v1",
				Resource: "certificatesigningrequests",
				Name:     desireName,
			},
			KubeContent: &runtime.RawExtension{Raw: []byte(`{}`)},
		},
	}
}

// newTestIssuedCredentialWithDesires creates a credential in Phase=Issued that
// has outstanding ApplyDesire and ReadDesire refs (mimicking a credential that
// has been issued but not yet cleaned up).
func newTestIssuedCredentialWithDesires(credName string) *api.SystemAdminCredential {
	cred := newTestCredential(credName, api.SystemAdminCredentialPhaseIssued)
	cred.Status.SignedCertificate = "base64-cert-data"
	cred.Status.OutstandingDesires = []api.SystemAdminCredentialDesireRef{
		{Kind: api.SystemAdminCredentialDesireKindApply, Name: fmt.Sprintf("system-admin-credential-csr-%s", credName)},
		{Kind: api.SystemAdminCredentialDesireKindApply, Name: fmt.Sprintf("system-admin-credential-csra-%s", credName)},
		{Kind: api.SystemAdminCredentialDesireKindRead, Name: fmt.Sprintf("system-admin-credential-csr-%s", credName)},
	}
	return cred
}

func TestPostIssuanceCleanup_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()

	tests := []struct {
		name              string
		credentials       []*api.SystemAdminCredential
		kaResources       []any // seeded into kube-applier
		expectProcessed   int
		expectError       bool
		expectOutstanding int // remaining OutstandingDesires on first credential
	}{
		{
			name: "issued credential with outstanding desires gets cleaned up",
			credentials: []*api.SystemAdminCredential{
				newTestIssuedCredentialWithDesires(testCredentialName),
			},
			kaResources: []any{
				newTestApplyDesire(fmt.Sprintf("system-admin-credential-csr-%s", testCredentialName)),
				newTestApplyDesire(fmt.Sprintf("system-admin-credential-csra-%s", testCredentialName)),
				newTestReadDesireWithoutContent(testCredentialName),
			},
			expectProcessed:   1,
			expectError:       false,
			expectOutstanding: 0,
		},
		{
			name: "requested credential is left alone",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithOutstandingDesires(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			kaResources:       []any{},
			expectProcessed:   0,
			expectError:       false,
			expectOutstanding: 1, // the ReadDesire ref stays
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			})
			require.NoError(t, err)

			for _, cred := range tt.credentials {
				seedCredential(ctx, t, mockResourcesDBClient, cred)
			}

			mockKAClient, err := databasetesting.NewMockKubeApplierDBClientWithResources(ctx, tt.kaResources)
			require.NoError(t, err)
			mockKAClients := databasetesting.NewMockKubeApplierDBClients()
			mockKAClients.Register(mcResourceID, mockKAClient)

			controller := NewPostIssuanceCleanup(
				mockResourcesDBClient, mockKAClients, testEnvIdentifier)

			processed, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectProcessed, processed, "processed count mismatch")

			// Verify credential's OutstandingDesires.
			if len(tt.credentials) > 0 {
				credCRUD := mockResourcesDBClient.HCPClusters(
					testSubscriptionID, testResourceGroupName,
				).SystemAdminCredentials(testClusterName)
				updatedCred, err := credCRUD.Get(ctx, tt.credentials[0].CosmosMetadata.ResourceID.Name)
				require.NoError(t, err)
				assert.Len(t, updatedCred.Status.OutstandingDesires, tt.expectOutstanding,
					"outstanding desires count mismatch")
			}

			// For the happy path, verify DeleteDesires were created.
			if tt.expectProcessed > 0 {
				allDocs := mockKAClient.GetAllDocuments()
				// Original ApplyDesires (2) and ReadDesire (1) should be gone.
				// DeleteDesires (2) should have been created.
				assert.Equal(t, 2, len(allDocs),
					"should have 2 DeleteDesires remaining in kube-applier")
			}
		})
	}
}
