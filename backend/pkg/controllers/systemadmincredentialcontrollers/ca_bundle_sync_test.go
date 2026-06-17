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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const testCABundle = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

// newTestServingCAReadDesireWithSecret creates a serving-ca ReadDesire whose
// Status.KubeContent contains a mirrored Secret with the given CA data.
// If caData is empty, the Secret has no CA key.
func newTestServingCAReadDesireWithSecret(caData string) *kubeapplier.ReadDesire {
	desireName := fmt.Sprintf("serving-ca-%s", testClusterName)
	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		testSubscriptionID, testResourceGroupName, testClusterName, desireName)
	resourceID := api.Must(azcorearm.ParseResourceID(resourceIDStr))
	mcResourceID := newTestManagementClusterResourceID()

	desire := &kubeapplier.ReadDesire{
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

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      servingCASecretName,
			Namespace: "ocm-test-abc123",
		},
	}
	if caData != "" {
		secret.Data = map[string][]byte{
			"tls.crt": []byte(caData),
		}
	}

	secretJSON, err := json.Marshal(secret)
	if err != nil {
		panic(err)
	}
	desire.Status.KubeContent = &runtime.RawExtension{Raw: secretJSON}

	return desire
}

func TestCABundleSync_SyncCluster(t *testing.T) {
	mcResourceID := newTestManagementClusterResourceID()

	tests := []struct {
		name         string
		spcCABundle  string // pre-existing ServingCABundle on SPC
		kaResources  []any
		expectResult int
		expectError  bool
		expectBundle string // expected SPC.Status.ServingCABundle after sync
	}{
		{
			name:        "syncs CA bundle from mirrored secret to SPC status",
			spcCABundle: "",
			kaResources: []any{
				newTestServingCAReadDesireWithSecret(testCABundle),
			},
			expectResult: 1,
			expectError:  false,
			expectBundle: testCABundle,
		},
		{
			name:        "no change when bundle already matches",
			spcCABundle: testCABundle,
			kaResources: []any{
				newTestServingCAReadDesireWithSecret(testCABundle),
			},
			expectResult: 1,
			expectError:  false,
			expectBundle: testCABundle,
		},
		{
			name:         "no mirrored content is a no-op",
			spcCABundle:  "",
			kaResources:  []any{},
			expectResult: 0,
			expectError:  false,
			expectBundle: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			spc := newTestSPCWithMC(mcResourceID)
			spc.Status.ServingCABundle = tt.spcCABundle

			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, []any{
				newTestCluster(),
				spc,
			})
			require.NoError(t, err)

			mockKAClient, err := databasetesting.NewMockKubeApplierDBClientWithResources(ctx, tt.kaResources)
			require.NoError(t, err)
			mockKAClients := databasetesting.NewMockKubeApplierDBClients()
			mockKAClients.Register(mcResourceID, mockKAClient)

			controller := NewCABundleSync(
				mockResourcesDBClient, mockKAClients, testEnvIdentifier)

			result, err := controller.SyncCluster(ctx, testSubscriptionID, testResourceGroupName, testClusterName)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectResult, result, "result mismatch")

			// Verify SPC's ServingCABundle.
			spcCRUD := mockResourcesDBClient.ServiceProviderClusters(
				testSubscriptionID, testResourceGroupName, testClusterName)
			updatedSPC, err := spcCRUD.Get(ctx, api.ServiceProviderClusterResourceName)
			require.NoError(t, err)
			assert.Equal(t, tt.expectBundle, updatedSPC.Status.ServingCABundle,
				"ServingCABundle mismatch")
		})
	}
}
