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
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/systemadmincredential"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	testSubscriptionID      = "00000000-0000-0000-0000-000000000000"
	testResourceGroupName   = "test-rg"
	testClusterName         = "test-cluster"
	testClusterServiceIDStr = "/api/aro_hcp/v1alpha1/clusters/abc123"
	testCredentialName      = "cred-001"
	testEnvIdentifier       = "test"
)

func newTestClusterResourceID() *azcorearm.ResourceID {
	return api.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/" + testResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testClusterName))
}

func newTestManagementClusterResourceID() *azcorearm.ResourceID {
	return api.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/mc-rg" +
			"/providers/Microsoft.ContainerService/managedClusters/mc-cluster"))
}

func newTestCluster() *api.HCPOpenShiftCluster {
	clusterResourceID := newTestClusterResourceID()
	csID := api.Must(api.NewInternalID(testClusterServiceIDStr))
	return &api.HCPOpenShiftCluster{
		CosmosMetadata: arm.CosmosMetadata{
			ResourceID: clusterResourceID,
		},
		TrackedResource: arm.TrackedResource{
			Resource: arm.Resource{
				ID:   clusterResourceID,
				Name: testClusterName,
				Type: clusterResourceID.ResourceType.String(),
			},
		},
		ServiceProviderProperties: api.HCPOpenShiftClusterServiceProviderProperties{
			ClusterServiceID: &csID,
		},
	}
}

func newTestSPCWithMC(mcResourceID *azcorearm.ResourceID) *api.ServiceProviderCluster {
	spcResourceID := api.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/" + testResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testClusterName +
			"/serviceProviderClusters/default"))
	return &api.ServiceProviderCluster{
		CosmosMetadata: api.CosmosMetadata{ResourceID: spcResourceID},
		Status: api.ServiceProviderClusterStatus{
			ManagementClusterResourceID: mcResourceID,
		},
	}
}

func newTestSPCWithoutMC() *api.ServiceProviderCluster {
	return newTestSPCWithMC(nil)
}

func newTestCredential(name string, phase api.SystemAdminCredentialPhase) *api.SystemAdminCredential {
	clusterResourceID := newTestClusterResourceID()
	credResourceID := api.Must(azcorearm.ParseResourceID(
		clusterResourceID.String() + "/systemAdminCredentials/" + name))

	// Generate a real keypair since BuildCSR needs a parseable private key.
	_, privatePEM, err := systemadmincredential.GenerateKeypair()
	if err != nil {
		panic(err)
	}

	return &api.SystemAdminCredential{
		CosmosMetadata: api.CosmosMetadata{ResourceID: credResourceID},
		Spec: api.SystemAdminCredentialSpec{
			Username:      "test-admin",
			PrivateKeyPEM: string(privatePEM),
		},
		Status: api.SystemAdminCredentialStatus{
			Phase: phase,
		},
	}
}

// seedCredential adds a SystemAdminCredential to the mock DB via the CRUD
// (since mock_init.go's addResource switch doesn't cover this type yet).
func seedCredential(ctx context.Context, t *testing.T, db *databasetesting.MockResourcesDBClient, cred *api.SystemAdminCredential) {
	t.Helper()
	credCRUD := db.HCPClusters(testSubscriptionID, testResourceGroupName).SystemAdminCredentials(testClusterName)
	_, err := credCRUD.Create(ctx, cred, nil)
	require.NoError(t, err)
}

func TestDesiresCreator_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()

	tests := []struct {
		name               string
		resources          []any // seeded via NewMockResourcesDBClientWithResources
		credentials        []*api.SystemAdminCredential
		registerMC         bool // whether to register MC in kube-applier
		expectProcessed    int
		expectError        bool
		expectDesireCount  int  // total ApplyDesires + ReadDesires in kube-applier
		expectOutstandings bool // credential OutstandingDesires populated
	}{
		{
			name: "requested credential creates desires",
			resources: []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			},
			credentials: []*api.SystemAdminCredential{
				newTestCredential(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			registerMC:         true,
			expectProcessed:    1,
			expectError:        false,
			expectDesireCount:  9, // 8 ApplyDesires + 1 ReadDesire
			expectOutstandings: true,
		},
		{
			name: "no requested credentials is a no-op",
			resources: []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			},
			credentials: []*api.SystemAdminCredential{
				newTestCredential(testCredentialName, api.SystemAdminCredentialPhaseIssued),
			},
			registerMC:         true,
			expectProcessed:    0,
			expectError:        false,
			expectDesireCount:  0,
			expectOutstandings: false,
		},
		{
			name: "missing MC placement skips gracefully",
			resources: []any{
				newTestCluster(),
				newTestSPCWithoutMC(),
			},
			credentials: []*api.SystemAdminCredential{
				newTestCredential(testCredentialName, api.SystemAdminCredentialPhaseRequested),
			},
			registerMC:         true,
			expectProcessed:    0,
			expectError:        false,
			expectDesireCount:  0,
			expectOutstandings: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			// Seed the resources DB.
			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, tt.resources)
			require.NoError(t, err)

			// Seed credentials manually (not supported by NewMockResourcesDBClientWithResources).
			for _, cred := range tt.credentials {
				seedCredential(ctx, t, mockResourcesDBClient, cred)
			}

			// Set up the kube-applier mock.
			mockKAClient := databasetesting.NewMockKubeApplierDBClient()
			mockKAClients := databasetesting.NewMockKubeApplierDBClients()
			if tt.registerMC {
				mockKAClients.Register(mcResourceID, mockKAClient)
			}

			// Construct the controller.
			controller := NewDesiresCreator(
				mockResourcesDBClient,
				mockKAClients,
				testEnvIdentifier,
			)

			// Call SyncCluster.
			processed, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectProcessed, processed, "processed count mismatch")

			// Verify desires in kube-applier.
			allDocs := mockKAClient.GetAllDocuments()
			assert.Equal(t, tt.expectDesireCount, len(allDocs),
				"kube-applier desire count mismatch")

			// Verify OutstandingDesires on the credential.
			if tt.expectOutstandings && len(tt.credentials) > 0 {
				credCRUD := mockResourcesDBClient.HCPClusters(
					testSubscriptionID, testResourceGroupName,
				).SystemAdminCredentials(testClusterName)
				updatedCred, err := credCRUD.Get(ctx, tt.credentials[0].CosmosMetadata.ResourceID.Name)
				require.NoError(t, err)
				assert.NotEmpty(t, updatedCred.Status.OutstandingDesires,
					"credential should have OutstandingDesires populated")
				assert.Len(t, updatedCred.Status.OutstandingDesires, 9,
					"should have 9 desire refs (8 apply + 1 read)")
			}
		})
	}
}

func TestDesiresCreator_Idempotent(t *testing.T) {
	ctx := context.Background()
	ctx = utils.ContextWithLogger(ctx, testr.New(t))

	mcResourceID := newTestManagementClusterResourceID()

	mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, []any{
		newTestCluster(),
		newTestSPCWithMC(mcResourceID),
	})
	require.NoError(t, err)

	cred := newTestCredential(testCredentialName, api.SystemAdminCredentialPhaseRequested)
	seedCredential(ctx, t, mockResourcesDBClient, cred)

	mockKAClient := databasetesting.NewMockKubeApplierDBClient()
	mockKAClients := databasetesting.NewMockKubeApplierDBClients()
	mockKAClients.Register(mcResourceID, mockKAClient)

	controller := NewDesiresCreator(mockResourcesDBClient, mockKAClients, testEnvIdentifier)

	// First sync creates desires.
	processed, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	firstRunDesireCount := len(mockKAClient.GetAllDocuments())
	assert.Equal(t, 9, firstRunDesireCount)

	// Second sync should be a no-op: credential was already updated with
	// OutstandingDesires, so the controller skips re-creation.
	processed, err = controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)
	require.NoError(t, err)
	// The credential is still in Requested phase but OutstandingDesires are
	// already populated, so the second sync processes the credential but
	// creates 0 new desires.
	assert.Equal(t, 9, len(mockKAClient.GetAllDocuments()),
		"desire count should remain unchanged after second sync")
}
