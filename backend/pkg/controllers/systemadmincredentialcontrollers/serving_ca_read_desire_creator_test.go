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

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// newTestServingCAReadDesire creates the serving-ca ReadDesire that
// ServingCAReadDesireCreator would create, for pre-seeding in tests.
func newTestServingCAReadDesire() *kubeapplier.ReadDesire {
	desireName := fmt.Sprintf("serving-ca-%s", testClusterName)
	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		testSubscriptionID, testResourceGroupName, testClusterName, desireName)
	resourceID := api.Must(azcorearm.ParseResourceID(resourceIDStr))
	mcResourceID := newTestManagementClusterResourceID()

	return &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:     "",
				Version:   "v1",
				Resource:  "secrets",
				Namespace: "ocm-test-abc123",
				Name:      servingCASecretName,
			},
		},
	}
}

func TestServingCAReadDesireCreator_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()

	tests := []struct {
		name          string
		resources     []any
		kaResources   []any
		registerMC    bool
		expectResult  int
		expectError   bool
		expectDesires int // total documents in kube-applier after sync
	}{
		{
			name: "creates serving-ca ReadDesire when absent",
			resources: []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			},
			kaResources:   []any{},
			registerMC:    true,
			expectResult:  1,
			expectError:   false,
			expectDesires: 1,
		},
		{
			name: "idempotent when ReadDesire already exists",
			resources: []any{
				newTestCluster(),
				newTestSPCWithMC(mcResourceID),
			},
			kaResources: []any{
				newTestServingCAReadDesire(),
			},
			registerMC:    true,
			expectResult:  1,
			expectError:   false,
			expectDesires: 1, // no duplicate created
		},
		{
			name: "missing MC placement skips gracefully",
			resources: []any{
				newTestCluster(),
				newTestSPCWithoutMC(),
			},
			kaResources:   []any{},
			registerMC:    true,
			expectResult:  0,
			expectError:   false,
			expectDesires: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, tt.resources)
			require.NoError(t, err)

			mockKAClient, err := databasetesting.NewMockKubeApplierDBClientWithResources(ctx, tt.kaResources)
			require.NoError(t, err)
			mockKAClients := databasetesting.NewMockKubeApplierDBClients()
			if tt.registerMC {
				mockKAClients.Register(mcResourceID, mockKAClient)
			}

			controller := NewServingCAReadDesireCreator(
				mockResourcesDBClient, mockKAClients, testEnvIdentifier)

			result, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectResult, result, "result mismatch")

			allDocs := mockKAClient.GetAllDocuments()
			assert.Equal(t, tt.expectDesires, len(allDocs),
				"kube-applier desire count mismatch")
		})
	}
}
