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

package identity

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"

	azureclient "github.com/Azure/ARO-HCP/backend/pkg/azure/client"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/internal/apitesting/coreapitesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func TestDesiredDataPlaneOperatorResourceIDsMatchSPC(t *testing.T) {
	t.Parallel()

	identityA := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-a"))
	identityB := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-b"))
	mixedCaseIdentityA := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/Test-RG/providers/Microsoft.ManagedIdentity/userAssignedIdentities/Identity-A"))

	testCases := []struct {
		name               string
		desiredResourceIDs map[string]struct{}
		spcIdentities      map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity
		expectedMatch      bool
	}{
		{
			name:               "both empty match",
			desiredResourceIDs: map[string]struct{}{},
			spcIdentities:      map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{},
			expectedMatch:      true,
		},
		{
			name: "matching resource ID",
			desiredResourceIDs: map[string]struct{}{
				strings.ToLower(identityA.String()): {},
			},
			spcIdentities: map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
				strings.ToLower(identityA.String()): {
					ResourceID: identityA,
				},
			},
			expectedMatch: true,
		},
		{
			name: "matching ignores resource ID casing when already lowercased as key",
			desiredResourceIDs: map[string]struct{}{
				strings.ToLower(mixedCaseIdentityA.String()): {},
			},
			spcIdentities: map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
				strings.ToLower(identityA.String()): {
					ResourceID: identityA,
				},
			},
			expectedMatch: true,
		},
		{
			name: "unique identity count mismatch",
			desiredResourceIDs: map[string]struct{}{
				strings.ToLower(identityA.String()): {},
			},
			spcIdentities: map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
				strings.ToLower(identityA.String()): {
					ResourceID: identityA,
				},
				strings.ToLower(identityB.String()): {
					ResourceID: identityB,
				},
			},
			expectedMatch: false,
		},
		{
			name: "resource ID mismatch",
			desiredResourceIDs: map[string]struct{}{
				strings.ToLower(identityA.String()): {},
			},
			spcIdentities: map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
				strings.ToLower(identityB.String()): {
					ResourceID: identityB,
				},
			},
			expectedMatch: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			syncer := &fetchDataPlaneOperatorsManagedIdentitiesInfoSyncer{}
			spc := &coreapi.ServiceProviderCluster{}
			spc.Status.DataPlaneOperatorsManagedIdentities.Identities = tc.spcIdentities

			assert.Equal(t, tc.expectedMatch, syncer.desiredDataPlaneOperatorResourceIDsMatchSPC(tc.desiredResourceIDs, spc))
		})
	}
}

func TestUniqueDataPlaneOperatorResourceIDs(t *testing.T) {
	t.Parallel()

	identityA := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-a"))
	mixedCaseIdentityA := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/Test-RG/providers/Microsoft.ManagedIdentity/userAssignedIdentities/Identity-A"))

	syncer := &fetchDataPlaneOperatorsManagedIdentitiesInfoSyncer{}

	t.Run("dedupes shared identity across operators", func(t *testing.T) {
		t.Parallel()
		unique := syncer.uniqueDataPlaneOperatorResourceIDs(map[string]*azcorearm.ResourceID{
			"operator-a": identityA,
			"operator-b": identityA,
		})
		require.NotNil(t, unique)
		assert.Equal(t, map[string]struct{}{
			strings.ToLower(identityA.String()): {},
		}, unique)
	})

	t.Run("lowercases resource ID keys", func(t *testing.T) {
		t.Parallel()
		unique := syncer.uniqueDataPlaneOperatorResourceIDs(map[string]*azcorearm.ResourceID{
			"operator-a": mixedCaseIdentityA,
		})
		require.NotNil(t, unique)
		assert.Equal(t, map[string]struct{}{
			strings.ToLower(identityA.String()): {},
		}, unique)
	})

	t.Run("nil resource ID returns nil", func(t *testing.T) {
		t.Parallel()
		unique := syncer.uniqueDataPlaneOperatorResourceIDs(map[string]*azcorearm.ResourceID{
			"operator-a": nil,
		})
		assert.Nil(t, unique)
	})
}

func TestFetchDataPlaneOperatorsManagedIdentitiesInfoNeedsWork(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	identityA := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-a"))
	identityB := metadataapi.Must(azcorearm.ParseResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-b"))

	matchingDesired := map[string]struct{}{
		strings.ToLower(identityA.String()): {},
	}
	matchingSPCIdentities := map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
		strings.ToLower(identityA.String()): {
			ResourceID: identityA,
		},
	}

	testCases := []struct {
		name                string
		desiredResourceIDs  map[string]struct{}
		spcIdentities       map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity
		earliestRecheckTime *metav1.Time
		expectedNeedsWork   bool
	}{
		{
			name:                "matching identities with future recheck skips work",
			desiredResourceIDs:  matchingDesired,
			spcIdentities:       matchingSPCIdentities,
			earliestRecheckTime: &metav1.Time{Time: now.Add(time.Hour)},
			expectedNeedsWork:   false,
		},
		{
			name:                "matching identities with past recheck needs work",
			desiredResourceIDs:  matchingDesired,
			spcIdentities:       matchingSPCIdentities,
			earliestRecheckTime: &metav1.Time{Time: now.Add(-time.Hour)},
			expectedNeedsWork:   true,
		},
		{
			name:                "matching identities with nil recheck needs work",
			desiredResourceIDs:  matchingDesired,
			spcIdentities:       matchingSPCIdentities,
			earliestRecheckTime: nil,
			expectedNeedsWork:   true,
		},
		{
			name: "mismatched identities ignore future recheck",
			desiredResourceIDs: map[string]struct{}{
				strings.ToLower(identityB.String()): {},
			},
			spcIdentities:       matchingSPCIdentities,
			earliestRecheckTime: &metav1.Time{Time: now.Add(time.Hour)},
			expectedNeedsWork:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			syncer := &fetchDataPlaneOperatorsManagedIdentitiesInfoSyncer{
				clock: clocktesting.NewFakePassiveClock(now),
			}
			spc := &coreapi.ServiceProviderCluster{}
			spc.Status.DataPlaneOperatorsManagedIdentities.Identities = tc.spcIdentities
			spc.Status.DataPlaneOperatorsManagedIdentities.EarliestRecheckTime = tc.earliestRecheckTime

			require.Equal(t, tc.expectedNeedsWork, syncer.needsWork(spc, tc.desiredResourceIDs))
		})
	}
}

// fakeUAIGetResult is a canned response for a single fakeUserAssignedIdentitiesClient.Get call.
type fakeUAIGetResult struct {
	response armmsi.UserAssignedIdentitiesClientGetResponse
	err      error
}

// fakeUserAssignedIdentitiesClient is a hand-rolled azureclient.UserAssignedIdentitiesClient
// used by the SyncOnce tests. Only Get is exercised; CreateOrUpdate and Delete exist to satisfy
// the interface and fail loudly if ever called. Get responses are keyed by the lowercased Azure
// resource name of the identity.
type fakeUserAssignedIdentitiesClient struct {
	getResponses map[string]fakeUAIGetResult
	getCallCount map[string]int
}

var _ azureclient.UserAssignedIdentitiesClient = (*fakeUserAssignedIdentitiesClient)(nil)

func (f *fakeUserAssignedIdentitiesClient) Get(_ context.Context, _ string, resourceName string, _ *armmsi.UserAssignedIdentitiesClientGetOptions) (armmsi.UserAssignedIdentitiesClientGetResponse, error) {
	if f.getCallCount == nil {
		f.getCallCount = map[string]int{}
	}
	f.getCallCount[strings.ToLower(resourceName)]++
	result, ok := f.getResponses[strings.ToLower(resourceName)]
	if !ok {
		return armmsi.UserAssignedIdentitiesClientGetResponse{}, fmt.Errorf("unexpected Get call for identity %q", resourceName)
	}
	return result.response, result.err
}

func (f *fakeUserAssignedIdentitiesClient) CreateOrUpdate(_ context.Context, _ string, _ string, _ armmsi.Identity, _ *armmsi.UserAssignedIdentitiesClientCreateOrUpdateOptions) (armmsi.UserAssignedIdentitiesClientCreateOrUpdateResponse, error) {
	return armmsi.UserAssignedIdentitiesClientCreateOrUpdateResponse{}, fmt.Errorf("CreateOrUpdate is not implemented by the fake")
}

func (f *fakeUserAssignedIdentitiesClient) Delete(_ context.Context, _ string, _ string, _ *armmsi.UserAssignedIdentitiesClientDeleteOptions) (armmsi.UserAssignedIdentitiesClientDeleteResponse, error) {
	return armmsi.UserAssignedIdentitiesClientDeleteResponse{}, fmt.Errorf("Delete is not implemented by the fake")
}

// uaiGetResponse builds a successful Get response carrying the given ClientID and PrincipalID.
func uaiGetResponse(clientID, principalID string) armmsi.UserAssignedIdentitiesClientGetResponse {
	return armmsi.UserAssignedIdentitiesClientGetResponse{
		Identity: armmsi.Identity{
			Properties: &armmsi.UserAssignedIdentityProperties{
				ClientID:    ptr.To(clientID),
				PrincipalID: ptr.To(principalID),
			},
		},
	}
}

// resourceNotFoundError returns an Azure error recognized by azureclient.IsResourceNotFoundErr.
func resourceNotFoundError() error {
	return &azcore.ResponseError{ErrorCode: "ResourceNotFound"}
}

// syncTestClusterResourceID is the ARM resource ID shared by the SyncOnce test cluster.
func syncTestClusterResourceID() *azcorearm.ResourceID {
	return metadataapi.Must(azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID))
}

// newSyncTestCluster returns a valid HCPOpenShiftCluster wired for the SyncOnce tests. Callers can
// customize it (e.g. set DataPlaneOperators or a DeletionTimestamp) via functional opts.
func newSyncTestCluster(opts ...func(*coreapi.HCPOpenShiftCluster)) *coreapi.HCPOpenShiftCluster {
	rid := syncTestClusterResourceID()
	cluster := coreapitesting.MinimumValidClusterTestCase()
	cluster.CosmosMetadata = coreapi.CosmosMetadata{
		ResourceID:   rid,
		PartitionKey: strings.ToLower(rid.SubscriptionID),
	}
	cluster.ID = rid
	cluster.Name = rid.Name
	cluster.Type = rid.ResourceType.String()
	for _, opt := range opts {
		opt(cluster)
	}
	return cluster
}

// newSyncTestSPC returns a ServiceProviderCluster document for the SyncOnce test cluster.
func newSyncTestSPC(opts ...func(*coreapi.ServiceProviderCluster)) *coreapi.ServiceProviderCluster {
	clusterRID := syncTestClusterResourceID()
	resourceID := metadataapi.Must(azcorearm.ParseResourceID(fmt.Sprintf("%s/%s/%s",
		clusterRID.String(),
		coreapi.ServiceProviderClusterResourceTypeName,
		coreapi.ServiceProviderClusterResourceName,
	)))
	spc := &coreapi.ServiceProviderCluster{
		CosmosMetadata: coreapi.CosmosMetadata{ResourceID: resourceID},
	}
	spc.SetPartitionKey(clusterRID.SubscriptionID)
	for _, opt := range opts {
		opt(spc)
	}
	return spc
}

// withDataPlaneOperators sets the cluster's ServiceManagedIdentity and data plane operator identities.
func withDataPlaneOperators(smi *azcorearm.ResourceID, dataPlaneOperators map[string]*azcorearm.ResourceID) func(*coreapi.HCPOpenShiftCluster) {
	return func(c *coreapi.HCPOpenShiftCluster) {
		c.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.ServiceManagedIdentity = smi
		c.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators = dataPlaneOperators
	}
}

func TestFetchDataPlaneOperatorsManagedIdentitiesInfoSyncOnce(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	smiIdentity := coreapitesting.NewTestUserAssignedIdentity("service-managed-identity")
	identityA := coreapitesting.NewTestUserAssignedIdentity("dataplane-identity-a")
	identityAKey := strings.ToLower(identityA.String())

	tests := []struct {
		name        string
		dbCluster   *coreapi.HCPOpenShiftCluster
		existingSPC *coreapi.ServiceProviderCluster
		// callsAzure indicates whether the controller is expected to build a
		// UserAssignedIdentitiesClient and call Azure during the sync.
		callsAzure      bool
		builderErr      error
		uaiGetResponses map[string]fakeUAIGetResult
		expectError     bool
		verify          func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient)
	}{
		{
			name:        "no-op when the cluster does not exist",
			dbCluster:   nil,
			callsAzure:  false,
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				_, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				assert.Error(t, err, "ServiceProviderCluster should not have been created for a missing cluster")
			},
		},
		{
			name: "no-op when the cluster is being deleted",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
				func(c *coreapi.HCPOpenShiftCluster) {
					deletionTime := metav1.NewTime(now)
					c.ServiceProviderProperties.DeletionTimestamp = &deletionTime
				},
			),
			callsAzure:  false,
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				_, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				assert.Error(t, err, "ServiceProviderCluster should not be created while the cluster is deleting")
			},
		},
		{
			name: "returns an error when a data plane operator ResourceID is nil",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": nil}),
			),
			callsAzure:  false,
			expectError: true,
		},
		{
			name: "resolves ClientID and PrincipalID for a data plane operator identity",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			callsAzure: true,
			uaiGetResponses: map[string]fakeUAIGetResult{
				"dataplane-identity-a": {response: uaiGetResponse("client-a", "principal-a")},
			},
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				spc, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				require.NoError(t, err)
				identities := spc.Status.DataPlaneOperatorsManagedIdentities.Identities
				require.Contains(t, identities, identityAKey)
				resolved := identities[identityAKey]
				require.NotNil(t, resolved)
				require.NotNil(t, resolved.ResourceID)
				// The controller parses the ResourceID from the lowercased dedup key, so the stored
				// value round-trips to the same lowercased key.
				assert.Equal(t, identityAKey, strings.ToLower(resolved.ResourceID.String()))
				require.NotNil(t, resolved.ClientID)
				assert.Equal(t, "client-a", *resolved.ClientID)
				require.NotNil(t, resolved.PrincipalID)
				assert.Equal(t, "principal-a", *resolved.PrincipalID)
				require.NotNil(t, spc.Status.DataPlaneOperatorsManagedIdentities.EarliestRecheckTime, "EarliestRecheckTime should be set after a successful sync")
				assert.True(t, spc.Status.DataPlaneOperatorsManagedIdentities.EarliestRecheckTime.After(now), "EarliestRecheckTime should be in the future")
			},
		},
		{
			name: "deduplicates an identity shared across operators and calls Azure once",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{
					"operator-a": identityA,
					"operator-b": identityA,
				}),
			),
			callsAzure: true,
			uaiGetResponses: map[string]fakeUAIGetResult{
				"dataplane-identity-a": {response: uaiGetResponse("client-a", "principal-a")},
			},
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				spc, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				require.NoError(t, err)
				assert.Len(t, spc.Status.DataPlaneOperatorsManagedIdentities.Identities, 1, "shared identity should be deduplicated to a single entry")
			},
		},
		{
			name: "keeps the identity but clears ClientID/PrincipalID when Azure reports ResourceNotFound",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			callsAzure: true,
			uaiGetResponses: map[string]fakeUAIGetResult{
				"dataplane-identity-a": {err: resourceNotFoundError()},
			},
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				spc, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				require.NoError(t, err)
				resolved := spc.Status.DataPlaneOperatorsManagedIdentities.Identities[identityAKey]
				require.NotNil(t, resolved, "identity should be retained even when Azure cannot find it")
				assert.Nil(t, resolved.ClientID, "ClientID should be nil for a not-found identity")
				assert.Nil(t, resolved.PrincipalID, "PrincipalID should be nil for a not-found identity")
				require.NotNil(t, spc.Status.DataPlaneOperatorsManagedIdentities.EarliestRecheckTime, "EarliestRecheckTime should still be set when identities are only not-found")
			},
		},
		{
			name: "returns an error and does not set the recheck time when an Azure Get fails",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			callsAzure: true,
			uaiGetResponses: map[string]fakeUAIGetResult{
				"dataplane-identity-a": {err: fmt.Errorf("azure is unavailable")},
			},
			expectError: true,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				spc, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				require.NoError(t, err)
				assert.Nil(t, spc.Status.DataPlaneOperatorsManagedIdentities.EarliestRecheckTime, "EarliestRecheckTime must not advance when a Get failed")
			},
		},
		{
			name: "returns an error when Azure returns an identity with nil Properties",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			callsAzure: true,
			uaiGetResponses: map[string]fakeUAIGetResult{
				"dataplane-identity-a": {response: armmsi.UserAssignedIdentitiesClientGetResponse{Identity: armmsi.Identity{Properties: nil}}},
			},
			expectError: true,
		},
		{
			name: "returns an error when the User Assigned Identities client cannot be built",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			callsAzure:  true,
			builderErr:  fmt.Errorf("failed to build client"),
			expectError: true,
		},
		{
			name: "skips Azure when identities already match and the recheck time is in the future",
			dbCluster: newSyncTestCluster(
				withDataPlaneOperators(smiIdentity, map[string]*azcorearm.ResourceID{"operator-a": identityA}),
			),
			existingSPC: newSyncTestSPC(func(spc *coreapi.ServiceProviderCluster) {
				futureRecheck := metav1.NewTime(now.Add(time.Hour))
				spc.Status.DataPlaneOperatorsManagedIdentities = coreapi.ServiceProviderClusterDataPlaneOperatorsManagedIdentities{
					Identities: map[string]*coreapi.ServiceProviderClusterDataPlaneOperatorManagedIdentity{
						identityAKey: {
							ResourceID:  identityA,
							ClientID:    ptr.To("previously-resolved-client"),
							PrincipalID: ptr.To("previously-resolved-principal"),
						},
					},
					EarliestRecheckTime: &futureRecheck,
				}
			}),
			callsAzure:  false,
			expectError: false,
			verify: func(t *testing.T, ctx context.Context, db *corecosmosstoragetesting.MockResourcesDBClient) {
				rid := syncTestClusterResourceID()
				spc, err := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
				require.NoError(t, err)
				resolved := spc.Status.DataPlaneOperatorsManagedIdentities.Identities[identityAKey]
				require.NotNil(t, resolved)
				require.NotNil(t, resolved.ClientID)
				assert.Equal(t, "previously-resolved-client", *resolved.ClientID, "existing values must be left untouched when no work is needed")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))

			seededResources := []any{}
			if tt.dbCluster != nil {
				seededResources = append(seededResources, tt.dbCluster)
			}
			if tt.existingSPC != nil {
				seededResources = append(seededResources, tt.existingSPC)
			}
			mockDB, err := corecosmosstoragetesting.NewMockResourcesDBClientWithResources(ctx, seededResources)
			require.NoError(t, err)

			ctrl := gomock.NewController(t)
			smiClientBuilder := azureclient.NewMockServiceManagedIdentityClientBuilder(ctrl)
			if tt.callsAzure {
				if tt.builderErr != nil {
					smiClientBuilder.EXPECT().
						UserAssignedIdentitiesClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
						Return(nil, tt.builderErr).
						Times(1)
				} else {
					fakeClient := &fakeUserAssignedIdentitiesClient{getResponses: tt.uaiGetResponses}
					smiClientBuilder.EXPECT().
						UserAssignedIdentitiesClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
						Return(fakeClient, nil).
						Times(1)
				}
			}

			syncer := &fetchDataPlaneOperatorsManagedIdentitiesInfoSyncer{
				clock:             clocktesting.NewFakePassiveClock(now),
				resourcesDBClient: mockDB,
				smiClientBuilder:  smiClientBuilder,
			}

			key := controllerutils.HCPClusterKey{
				SubscriptionID:    coreapitesting.TestSubscriptionID,
				ResourceGroupName: coreapitesting.TestResourceGroupName,
				HCPClusterName:    coreapitesting.TestClusterName,
			}

			err = syncer.SyncOnce(ctx, key)
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tt.verify != nil {
				tt.verify(t, ctx, mockDB)
			}
		})
	}
}
