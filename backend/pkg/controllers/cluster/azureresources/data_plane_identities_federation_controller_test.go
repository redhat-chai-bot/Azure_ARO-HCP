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
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"

	"github.com/Azure/ARO-HCP/internal/azure"

	azureclient "github.com/Azure/ARO-HCP/backend/pkg/azure/client"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/corelistertesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	testFICSubscriptionID    = "00000000-0000-0000-0000-000000000001"
	testFICResourceGroupName = "test-rg"
	testFICClusterName       = "test-cluster"
	testFICTenantID          = "test-tenant-id"
	testFICIssuerURL         = "https://oidc.example.com/cluster123"
	testFICOperatorName      = "disk-csi-driver"
	testFICSANamespace       = "openshift-cluster-csi-drivers"
	testFICSAName            = "azure-disk-csi-driver-controller-sa"
)

var (
	testFICIdentityResourceID = metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testFICSubscriptionID +
			"/resourceGroups/managed-rg" +
			"/providers/Microsoft.ManagedIdentity/userAssignedIdentities/disk-csi-driver-identity",
	))
)

// newTestFICCluster builds an HCPOpenShiftCluster for FIC tests.
func newTestFICCluster(deleting bool, issuerURL string, dataPlaneOperators map[string]*azcorearm.ResourceID) *coreapi.HCPOpenShiftCluster {
	resourceID := metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testFICSubscriptionID +
			"/resourceGroups/" + testFICResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testFICClusterName,
	))

	cluster := &coreapi.HCPOpenShiftCluster{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(resourceID.SubscriptionID),
		},
		TrackedResource: coreapi.TrackedResource{
			Resource: coreapi.Resource{
				ID:   resourceID,
				Name: testFICClusterName,
				Type: resourceID.ResourceType.String(),
			},
		},
	}
	cluster.ServiceProviderProperties.Platform.IssuerURL = issuerURL
	cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators = dataPlaneOperators
	if deleting {
		cluster.ServiceProviderProperties.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	return cluster
}

// newTestFICSPC builds a ServiceProviderCluster with the given FIC tracking state.
func newTestFICSPC(ficTracking coreapi.DataPlaneIdentitiesFederatedCredentials) *coreapi.ServiceProviderCluster {
	resourceID := metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testFICSubscriptionID +
			"/resourceGroups/" + testFICResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testFICClusterName +
			"/" + coreapi.ServiceProviderClusterResourceTypeName +
			"/" + coreapi.ServiceProviderClusterResourceName,
	))

	spc := &coreapi.ServiceProviderCluster{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(resourceID.SubscriptionID),
		},
	}
	spc.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials = ficTracking
	return spc
}

// newTestFICSubscription builds a Subscription for testing.
func newTestFICSubscription() *coreapi.Subscription {
	subscriptionResourceID := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/" + testFICSubscriptionID))
	return &coreapi.Subscription{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   subscriptionResourceID,
			PartitionKey: strings.ToLower(subscriptionResourceID.SubscriptionID),
		},
		ResourceID: subscriptionResourceID,
		Properties: &coreapi.SubscriptionProperties{
			TenantId: ptr.To(testFICTenantID),
		},
	}
}

// testClusterScopedIdentitiesConfig returns a config with one data plane operator.
func testClusterScopedIdentitiesConfig() *azure.ClusterScopedIdentitiesConfig {
	return &azure.ClusterScopedIdentitiesConfig{
		DataPlaneOperatorsIdentities: azure.DataPlaneOperatorsIdentities{
			azure.ClusterOperatorIdentifier(testFICOperatorName): &azure.DataPlaneOperatorIdentity{
				KubernetesServiceAccounts: []*azure.KubernetesServiceAccount{
					{
						Namespace: testFICSANamespace,
						Name:      testFICSAName,
					},
				},
			},
		},
	}
}

// ficNotFoundError returns an *azcore.ResponseError for a missing FIC.
func ficNotFoundError() *azcore.ResponseError {
	return &azcore.ResponseError{
		ErrorCode:  "FederatedIdentityCredentialNotFound",
		StatusCode: http.StatusNotFound,
		RawResponse: &http.Response{
			Status:     "404 Not Found",
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"FederatedIdentityCredentialNotFound","message":"Not found."}}`)),
			Request: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Scheme: "https", Host: "management.azure.com", Path: "/fic"},
			},
		},
	}
}

// ficGetResponse returns a FIC Get response with the given properties.
func ficGetResponse(issuer, subject, audience string) armmsi.FederatedIdentityCredentialsClientGetResponse {
	return armmsi.FederatedIdentityCredentialsClientGetResponse{
		FederatedIdentityCredential: armmsi.FederatedIdentityCredential{
			Properties: &armmsi.FederatedIdentityCredentialProperties{
				Issuer:    to.Ptr(issuer),
				Subject:   to.Ptr(subject),
				Audiences: []*string{to.Ptr(audience)},
			},
		},
	}
}

func TestGenerateFICName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		clusterName  string
		operatorName string
		saNamespace  string
		saName       string
	}{
		{
			name:         "standard inputs",
			clusterName:  "my-cluster",
			operatorName: "disk-csi-driver",
			saNamespace:  "openshift-cluster-csi-drivers",
			saName:       "azure-disk-csi-driver-controller-sa",
		},
		{
			name:         "different cluster produces different name",
			clusterName:  "other-cluster",
			operatorName: "disk-csi-driver",
			saNamespace:  "openshift-cluster-csi-drivers",
			saName:       "azure-disk-csi-driver-controller-sa",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := GenerateFICName(tc.clusterName, tc.operatorName, tc.saNamespace, tc.saName)
			assert.True(t, len(result) >= 3, "FIC name should be at least 3 characters long")
			assert.True(t, len(result) <= 120, "FIC name should be at most 120 characters long")
			assert.True(t, strings.HasPrefix(result, "fic-"), "FIC name should start with 'fic-'")

			// Deterministic: same inputs produce same output.
			result2 := GenerateFICName(tc.clusterName, tc.operatorName, tc.saNamespace, tc.saName)
			assert.Equal(t, result, result2, "FIC name should be deterministic")
		})
	}

	// Different inputs produce different names.
	name1 := GenerateFICName("cluster-a", "op1", "ns1", "sa1")
	name2 := GenerateFICName("cluster-b", "op1", "ns1", "sa1")
	assert.NotEqual(t, name1, name2, "different clusters should produce different FIC names")
}

func TestDataPlaneIdentitiesFederationNeedsWork(t *testing.T) {
	t.Parallel()

	expectedSubject := (&azure.KubernetesServiceAccount{
		Namespace: testFICSANamespace,
		Name:      testFICSAName,
	}).AsOIDCSubject()

	expectedFICName := GenerateFICName(testFICClusterName, testFICOperatorName, testFICSANamespace, testFICSAName)

	dataPlaneOperators := map[string]*azcorearm.ResourceID{
		testFICOperatorName: testFICIdentityResourceID,
	}

	testCases := []struct {
		name        string
		deleting    bool
		operators   map[string]*azcorearm.ResourceID
		ficTracking coreapi.DataPlaneIdentitiesFederatedCredentials
		expect      bool
	}{
		{
			name:        "not deleting, no operators configured, no work needed",
			deleting:    false,
			operators:   nil,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			expect:      false,
		},
		{
			name:        "not deleting, operators configured but no tracking yet, needs work",
			deleting:    false,
			operators:   dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			expect:      true,
		},
		{
			name:      "not deleting, all FICs confirmed, no work needed",
			deleting:  false,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName:   expectedFICName,
								Confirmed: true,
							},
						},
					},
				},
			},
			expect: false,
		},
		{
			name:      "not deleting, FIC still pending, needs work",
			deleting:  false,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName: expectedFICName,
								Pending: true,
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name:      "deleting with tracked operators, needs work",
			deleting:  true,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName:   expectedFICName,
								Confirmed: true,
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name:        "deleting with no tracked operators, no work needed",
			deleting:    true,
			operators:   dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			expect:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			syncer := &dataPlaneIdentitiesFederationSyncer{
				clusterScopedIdentitiesConfig: testClusterScopedIdentitiesConfig(),
			}

			cluster := newTestFICCluster(tc.deleting, testFICIssuerURL, tc.operators)
			spc := newTestFICSPC(tc.ficTracking)

			got := syncer.NeedsWork(cluster, spc)
			assert.Equal(t, tc.expect, got, "NeedsWork result")
		})
	}
}

func TestDataPlaneIdentitiesFederationSyncOnce(t *testing.T) {
	t.Parallel()

	expectedSubject := (&azure.KubernetesServiceAccount{
		Namespace: testFICSANamespace,
		Name:      testFICSAName,
	}).AsOIDCSubject()

	expectedFICName := GenerateFICName(testFICClusterName, testFICOperatorName, testFICSANamespace, testFICSAName)

	dataPlaneOperators := map[string]*azcorearm.ResourceID{
		testFICOperatorName: testFICIdentityResourceID,
	}

	testCases := []struct {
		name              string
		deleting          bool
		issuerURL         string
		operators         map[string]*azcorearm.ResourceID
		ficTracking       coreapi.DataPlaneIdentitiesFederatedCredentials
		setupMock         func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient
		expectErr         bool
		expectErrContains string
		expectConfirmed   bool
		expectCleared     bool
	}{
		{
			name:      "create FIC when it does not exist",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Get(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(armmsi.FederatedIdentityCredentialsClientGetResponse{}, ficNotFoundError()).
					Times(1)
				mock.EXPECT().
					CreateOrUpdate(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, gomock.Any(), nil).
					Return(armmsi.FederatedIdentityCredentialsClientCreateOrUpdateResponse{}, nil).
					Times(1)
				return mock
			},
			expectConfirmed: true,
		},
		{
			name:      "validate existing FIC with correct properties",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Get(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(ficGetResponse(testFICIssuerURL, expectedSubject, ficAudience), nil).
					Times(1)
				return mock
			},
			expectConfirmed: true,
		},
		{
			name:      "update existing FIC with wrong issuer",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Get(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(ficGetResponse("https://wrong-issuer.example.com", expectedSubject, ficAudience), nil).
					Times(1)
				mock.EXPECT().
					CreateOrUpdate(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, gomock.Any(), nil).
					Return(armmsi.FederatedIdentityCredentialsClientCreateOrUpdateResponse{}, nil).
					Times(1)
				return mock
			},
			expectConfirmed: true,
		},
		{
			name:      "update existing FIC with wrong subject",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Get(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(ficGetResponse(testFICIssuerURL, "system:serviceaccount:wrong-ns:wrong-sa", ficAudience), nil).
					Times(1)
				mock.EXPECT().
					CreateOrUpdate(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, gomock.Any(), nil).
					Return(armmsi.FederatedIdentityCredentialsClientCreateOrUpdateResponse{}, nil).
					Times(1)
				return mock
			},
			expectConfirmed: true,
		},
		{
			name:      "update existing FIC with wrong audience",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Get(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(ficGetResponse(testFICIssuerURL, expectedSubject, "wrong-audience"), nil).
					Times(1)
				mock.EXPECT().
					CreateOrUpdate(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, gomock.Any(), nil).
					Return(armmsi.FederatedIdentityCredentialsClientCreateOrUpdateResponse{}, nil).
					Times(1)
				return mock
			},
			expectConfirmed: true,
		},
		{
			name:              "error when issuer URL is empty",
			deleting:          false,
			issuerURL:         "",
			operators:         dataPlaneOperators,
			ficTracking:       coreapi.DataPlaneIdentitiesFederatedCredentials{},
			setupMock:         nil, // No Azure calls expected.
			expectErr:         true,
			expectErrContains: "issuer URL is empty",
		},
		{
			name:      "delete FIC during cluster deletion",
			deleting:  true,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName:   expectedFICName,
								Confirmed: true,
							},
						},
					},
				},
			},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Delete(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(armmsi.FederatedIdentityCredentialsClientDeleteResponse{}, nil).
					Times(1)
				return mock
			},
			expectCleared: true,
		},
		{
			name:      "delete FIC handles already deleted (not found)",
			deleting:  true,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName:   expectedFICName,
								Confirmed: true,
							},
						},
					},
				},
			},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				mock := azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
				mock.EXPECT().
					Delete(gomock.Any(), testFICIdentityResourceID.ResourceGroupName, testFICIdentityResourceID.Name, expectedFICName, nil).
					Return(armmsi.FederatedIdentityCredentialsClientDeleteResponse{}, ficNotFoundError()).
					Times(1)
				return mock
			},
			expectCleared: true,
		},
		{
			name:      "skip already confirmed FIC",
			deleting:  false,
			issuerURL: testFICIssuerURL,
			operators: dataPlaneOperators,
			ficTracking: coreapi.DataPlaneIdentitiesFederatedCredentials{
				Operators: map[string]*coreapi.DataPlaneOperatorFederatedCredentials{
					testFICOperatorName: {
						ServiceAccounts: map[string]*coreapi.FederatedIdentityCredentialReference{
							expectedSubject: {
								FICName:   expectedFICName,
								Confirmed: true,
							},
						},
					},
				},
			},
			setupMock: func(ctrl *gomock.Controller) azureclient.FederatedIdentityCredentialsClient {
				// No Azure calls expected since FIC is already confirmed.
				return azureclient.NewMockFederatedIdentityCredentialsClient(ctrl)
			},
			expectConfirmed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))

			cluster := newTestFICCluster(tc.deleting, tc.issuerURL, tc.operators)
			spc := newTestFICSPC(tc.ficTracking)

			mockResourcesDB, err := corecosmosstoragetesting.NewMockResourcesDBClientWithResources(ctx, []any{cluster, spc})
			require.NoError(t, err)

			ctrl := gomock.NewController(t)

			fpaClientBuilder := azureclient.NewMockFirstPartyApplicationClientBuilder(ctrl)

			if tc.setupMock != nil {
				mockFICClient := tc.setupMock(ctrl)
				fpaClientBuilder.EXPECT().
					FederatedIdentityCredentialsClient(testFICTenantID, testFICSubscriptionID).
					Return(mockFICClient, nil).
					AnyTimes()
			}

			syncer := &dataPlaneIdentitiesFederationSyncer{
				resourcesDBClient:             mockResourcesDB,
				clusterLister:                 &corelistertesting.DBClusterLister{ResourcesDBClient: mockResourcesDB},
				serviceProviderClusterLister:  &corelistertesting.DBServiceProviderClusterLister{ResourcesDBClient: mockResourcesDB},
				subscriptionLister:            &corelistertesting.SliceSubscriptionLister{Subscriptions: []*coreapi.Subscription{newTestFICSubscription()}},
				azureFPAClientBuilder:         fpaClientBuilder,
				clusterScopedIdentitiesConfig: testClusterScopedIdentitiesConfig(),
			}

			key := controllerutils.HCPClusterKey{
				SubscriptionID:    testFICSubscriptionID,
				ResourceGroupName: testFICResourceGroupName,
				HCPClusterName:    testFICClusterName,
			}

			syncErr := syncer.SyncOnce(ctx, key)
			if tc.expectErr {
				require.Error(t, syncErr)
				if tc.expectErrContains != "" {
					assert.Contains(t, syncErr.Error(), tc.expectErrContains)
				}
				return
			}
			require.NoError(t, syncErr)

			updated, err := mockResourcesDB.ServiceProviderClusters(testFICSubscriptionID, testFICResourceGroupName, testFICClusterName).Get(ctx, coreapi.ServiceProviderClusterResourceName)
			require.NoError(t, err)

			gotFICTracking := updated.Status.AzureResources.DataPlaneIdentitiesFederatedCredentials

			if tc.expectCleared {
				assert.Empty(t, gotFICTracking.Operators, "FIC tracking should be cleared after deletion")
				return
			}

			if tc.expectConfirmed {
				require.NotNil(t, gotFICTracking.Operators, "operators tracking should not be nil")
				operatorTracking, ok := gotFICTracking.Operators[testFICOperatorName]
				require.True(t, ok, "operator tracking should exist for %s", testFICOperatorName)

				saRef, ok := operatorTracking.ServiceAccounts[expectedSubject]
				require.True(t, ok, "service account reference should exist for subject %s", expectedSubject)
				assert.True(t, saRef.Confirmed, "FIC should be confirmed")
				assert.False(t, saRef.Pending, "FIC should not be pending")
				assert.Equal(t, expectedFICName, saRef.FICName, "FIC name should match")
			}
		})
	}
}

func TestValidateFederatedIdentityCredential(t *testing.T) {
	t.Parallel()

	syncer := &dataPlaneIdentitiesFederationSyncer{}

	testCases := []struct {
		name     string
		fic      armmsi.FederatedIdentityCredential
		issuer   string
		subject  string
		expected bool
	}{
		{
			name: "valid FIC with matching properties",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    to.Ptr("https://issuer.example.com"),
					Subject:   to.Ptr("system:serviceaccount:ns:sa"),
					Audiences: []*string{to.Ptr("openshift")},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: true,
		},
		{
			name:     "nil properties",
			fic:      armmsi.FederatedIdentityCredential{},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
		{
			name: "wrong issuer",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    to.Ptr("https://wrong.example.com"),
					Subject:   to.Ptr("system:serviceaccount:ns:sa"),
					Audiences: []*string{to.Ptr("openshift")},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
		{
			name: "wrong subject",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    to.Ptr("https://issuer.example.com"),
					Subject:   to.Ptr("system:serviceaccount:other-ns:other-sa"),
					Audiences: []*string{to.Ptr("openshift")},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
		{
			name: "wrong audience",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    to.Ptr("https://issuer.example.com"),
					Subject:   to.Ptr("system:serviceaccount:ns:sa"),
					Audiences: []*string{to.Ptr("wrong-audience")},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
		{
			name: "empty audiences",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    to.Ptr("https://issuer.example.com"),
					Subject:   to.Ptr("system:serviceaccount:ns:sa"),
					Audiences: []*string{},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
		{
			name: "nil issuer",
			fic: armmsi.FederatedIdentityCredential{
				Properties: &armmsi.FederatedIdentityCredentialProperties{
					Issuer:    nil,
					Subject:   to.Ptr("system:serviceaccount:ns:sa"),
					Audiences: []*string{to.Ptr("openshift")},
				},
			},
			issuer:   "https://issuer.example.com",
			subject:  "system:serviceaccount:ns:sa",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := syncer.validateFederatedIdentityCredential(tc.fic, tc.issuer, tc.subject)
			assert.Equal(t, tc.expected, got)
		})
	}
}
