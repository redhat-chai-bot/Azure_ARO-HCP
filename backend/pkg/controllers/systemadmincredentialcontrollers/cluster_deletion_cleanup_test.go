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

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// newTestCredentialWithApplyAndReadDesires creates a credential in the given
// phase with both ApplyDesire and ReadDesire refs in OutstandingDesires.
func newTestCredentialWithApplyAndReadDesires(name string, phase api.SystemAdminCredentialPhase) *api.SystemAdminCredential {
	cred := newTestCredential(name, phase)
	cred.Status.OutstandingDesires = []api.SystemAdminCredentialDesireRef{
		{Kind: api.SystemAdminCredentialDesireKindApply, Name: fmt.Sprintf("system-admin-credential-csr-%s", name)},
		{Kind: api.SystemAdminCredentialDesireKindRead, Name: fmt.Sprintf("system-admin-credential-csr-%s", name)},
	}
	return cred
}

func TestClusterDeletionCleanup_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()

	tests := []struct {
		name                string
		credentials         []*api.SystemAdminCredential
		kaResources         []any // seeded into kube-applier
		expectOutstanding   int   // return value: credentials still with outstanding desires
		expectError         bool
		expectDeleteDesires int // DeleteDesires created in kube-applier
	}{
		{
			name: "all credentials cleaned regardless of phase",
			credentials: []*api.SystemAdminCredential{
				newTestCredentialWithApplyAndReadDesires("cred-requested", api.SystemAdminCredentialPhaseRequested),
				newTestCredentialWithApplyAndReadDesires("cred-issued", api.SystemAdminCredentialPhaseIssued),
				newTestCredentialWithApplyAndReadDesires("cred-revoked", api.SystemAdminCredentialPhaseRevoked),
			},
			kaResources: []any{
				// ApplyDesires and ReadDesires for all three credentials
				newTestApplyDesire(fmt.Sprintf("system-admin-credential-csr-%s", "cred-requested")),
				newTestReadDesireWithoutContent("cred-requested"),
				newTestApplyDesire(fmt.Sprintf("system-admin-credential-csr-%s", "cred-issued")),
				newTestReadDesireWithoutContent("cred-issued"),
				newTestApplyDesire(fmt.Sprintf("system-admin-credential-csr-%s", "cred-revoked")),
				newTestReadDesireWithoutContent("cred-revoked"),
			},
			// Each credential gets its ApplyDesire replaced by a DeleteDesire ref,
			// so each credential still has 1 outstanding (the DeleteDesire ref).
			expectOutstanding:   3,
			expectError:         false,
			expectDeleteDesires: 3,
		},
		{
			name:                "no credentials is a clean no-op returning 0 outstanding",
			credentials:         []*api.SystemAdminCredential{},
			kaResources:         []any{},
			expectOutstanding:   0,
			expectError:         false,
			expectDeleteDesires: 0,
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

			controller := NewClusterDeletionCleanup(
				mockResourcesDBClient, mockKAClients, testEnvIdentifier)

			outstanding, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectOutstanding, outstanding,
				"outstanding credential count mismatch")

			// Count DeleteDesires in kube-applier.
			// After cleanup: original ApplyDesires are deleted, ReadDesires are
			// deleted, and DeleteDesires are created in their place.
			allDocs := mockKAClient.GetAllDocuments()
			assert.Equal(t, tt.expectDeleteDesires, len(allDocs),
				"kube-applier document count mismatch (should be only DeleteDesires)")
		})
	}
}
