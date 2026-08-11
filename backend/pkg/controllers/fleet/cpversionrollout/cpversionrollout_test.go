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

package cpversionrollout

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/fleetcosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/corelistertesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/fleetlistertesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	testSubscriptionID    = "00000000-0000-0000-0000-000000000001"
	testResourceGroupName = "test-rg"
	testChannel           = "stable-4.21"
	testMinor             = "4.21"
)

func testContext(t *testing.T) context.Context {
	return utils.ContextWithLogger(context.Background(), testr.New(t))
}

func mustV(s string) semver.Version {
	return semver.MustParse(s)
}

func ptrV(s string) *semver.Version {
	v := semver.MustParse(s)
	return &v
}

func clusterResourceID(t *testing.T, name string) *azcorearm.ResourceID {
	t.Helper()
	return metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/" + testResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + name))
}

func newRollout(t *testing.T, channel string) *fleetapi.ControlPlaneVersionRollout {
	t.Helper()
	rid := metadataapi.Must(fleetapi.ToControlPlaneVersionRolloutResourceID(channel))
	return &fleetapi.ControlPlaneVersionRollout{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   rid,
			PartitionKey: strings.ToLower(channel),
		},
	}
}

// newSPC builds a ServiceProviderCluster with the given desired version and
// active versions (most recent first). Empty desired means unset.
func newSPC(t *testing.T, name, desired string, active ...string) *coreapi.ServiceProviderCluster {
	t.Helper()
	rid := metadataapi.Must(azcorearm.ParseResourceID(
		coreapi.ToServiceProviderClusterResourceIDString(testSubscriptionID, testResourceGroupName, name)))
	spc := &coreapi.ServiceProviderCluster{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   rid,
			PartitionKey: strings.ToLower(testSubscriptionID),
		},
	}
	if desired != "" {
		spc.Spec.ControlPlaneVersion.DesiredVersion = ptrV(desired)
	}
	for _, a := range active {
		spc.Status.ControlPlaneVersion.ActiveVersions = append(
			spc.Status.ControlPlaneVersion.ActiveVersions,
			coreapi.HCPClusterActiveVersion{Version: ptrV(a)},
		)
	}
	return spc
}

func withDegraded(spc *coreapi.ServiceProviderCluster) *coreapi.ServiceProviderCluster {
	apimeta.SetStatusCondition(&spc.Status.Conditions, metav1.Condition{
		Type:    coreapi.DegradedCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "StuckUpgrade",
		Message: "upgrade stuck",
	})
	return spc
}

func withPin(spc *coreapi.ServiceProviderCluster, exact, until string) *coreapi.ServiceProviderCluster {
	pin := &coreapi.ClusterPinnedVersion{}
	if exact != "" {
		pin.ExactVersion = ptrV(exact)
	}
	if until != "" {
		pin.UntilExactVersion = ptrV(until)
	}
	spc.Spec.PinnedVersion = pin
	return spc
}

// --- Status collector: collectCounts ---

func TestCollectCounts(t *testing.T) {
	spcs := []*coreapi.ServiceProviderCluster{
		newSPC(t, "achieved", "4.21.6", "4.21.6"),             // achieved + successful
		newSPC(t, "mismatch-old", "4.21.6", "4.21.5"),         // mismatched
		newSPC(t, "mismatch-none", "4.21.6"),                  // mismatched (no active)
		withDegraded(newSPC(t, "failed", "4.21.6", "4.21.5")), // mismatched + failed
		newSPC(t, "other-minor", "4.20.9", "4.20.9"),          // skipped (different minor)
		newSPC(t, "no-desired", ""),                           // skipped (no desired)
	}

	counts := collectCounts(spcs, testMinor)

	assert.Equal(t, int64(4), counts.ClusterCountByDesiredExactVersion["4.21.6"], "desired count")
	assert.Equal(t, int64(1), counts.ClusterCountByAchievedExactVersion["4.21.6"], "achieved count")
	assert.Equal(t, int64(1), counts.SuccessfulClusterCountByAchievedExactVersion["4.21.6"], "successful count")
	assert.Equal(t, int64(3), counts.MismatchedClusterCountByDesiredExactVersion["4.21.6"], "mismatched count")
	assert.Equal(t, int64(1), counts.FailedClusterCountByDesiredExactVersion["4.21.6"], "failed count")
	// Different minor is not counted.
	assert.NotContains(t, counts.ClusterCountByDesiredExactVersion, "4.20.9")
}

// --- Normal assignment: eligibleClusters ---

func TestEligibleClusters(t *testing.T) {
	best := mustV("4.21.6")
	spcs := []*coreapi.ServiceProviderCluster{
		newSPC(t, "below-unpinned", "4.21.5"),                           // eligible
		newSPC(t, "at-best", "4.21.6"),                                  // not (not below best)
		newSPC(t, "other-minor", "4.20.5"),                              // not (different minor)
		withPin(newSPC(t, "pin-expired", "4.21.5"), "4.21.5", "4.21.6"), // eligible (until <= best)
		withPin(newSPC(t, "pin-active", "4.21.5"), "4.21.5", "4.21.9"),  // not (until > best)
		newSPC(t, "no-desired", ""),                                     // not (no desired)
	}

	eligible := eligibleClusters(spcs, testMinor, best)

	names := map[string]bool{}
	for _, spc := range eligible {
		names[spc.CosmosMetadata.ResourceID.Parent.Name] = true
	}
	assert.Len(t, eligible, 2)
	assert.True(t, names["below-unpinned"], "below-unpinned should be eligible")
	assert.True(t, names["pin-expired"], "pin-expired should be eligible")
}

// --- Normal assignment: planRollout ---

func TestPlanRollout(t *testing.T) {
	best := "4.21.6"
	eligibleN := func(n int) []*coreapi.ServiceProviderCluster {
		var out []*coreapi.ServiceProviderCluster
		for i := 0; i < n; i++ {
			out = append(out, newSPC(t, "c"+string(rune('a'+i)), "4.21.5"))
		}
		return out
	}

	tests := []struct {
		name           string
		status         fleetapi.ControlPlaneVersionRolloutStatus
		eligible       []*coreapi.ServiceProviderCluster
		totalInChannel int
		wantAction     rolloutActionType
		wantStatus     metav1.ConditionStatus
		wantReason     string
		wantUpgradeLen int
	}{
		{
			name: "failure budget exceeded halts rollout",
			status: fleetapi.ControlPlaneVersionRolloutStatus{
				FailedClusterCountByDesiredExactVersion: map[string]int64{best: 3},
			},
			eligible:       eligibleN(5),
			totalInChannel: 10,
			wantAction:     rolloutActionFailed,
			wantStatus:     metav1.ConditionFalse,
			wantReason:     fleetapi.ControlPlaneVersionRolloutReasonFailed,
		},
		{
			name:           "no eligible clusters is stable",
			status:         fleetapi.ControlPlaneVersionRolloutStatus{},
			eligible:       nil,
			totalInChannel: 10,
			wantAction:     rolloutActionStable,
			wantStatus:     metav1.ConditionFalse,
			wantReason:     fleetapi.ControlPlaneVersionRolloutReasonStable,
		},
		{
			name:           "canary wave selects clusters",
			status:         fleetapi.ControlPlaneVersionRolloutStatus{},
			eligible:       eligibleN(5),
			totalInChannel: 10, // canaryTarget = ceil(5% of 10)=1 +2 = 3
			wantAction:     rolloutActionCanary,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     fleetapi.ControlPlaneVersionRolloutReasonProgressing,
			wantUpgradeLen: 3,
		},
		{
			name: "waiting for canaries when not yet ready",
			status: fleetapi.ControlPlaneVersionRolloutStatus{
				// atBest = mismatched+achieved = 3 (meets canaryTarget of 3) but successful=0
				MismatchedClusterCountByDesiredExactVersion: map[string]int64{best: 3},
			},
			eligible:       eligibleN(5),
			totalInChannel: 10,
			wantAction:     rolloutActionWaitCanary,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     fleetapi.ControlPlaneVersionRolloutReasonProgressing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rollout := newRollout(t, testChannel)
			rollout.Spec.BestExactVersion = ptrV(best)
			rollout.Status = tt.status

			plan := planRollout(rollout, tt.eligible, tt.totalInChannel, DefaultRolloutConfig())

			assert.Equal(t, tt.wantAction, plan.action, "action")
			assert.Equal(t, tt.wantStatus, plan.conditionStatus, "condition status")
			assert.Equal(t, tt.wantReason, plan.conditionReason, "condition reason")
			assert.Len(t, plan.toUpgrade, tt.wantUpgradeLen, "clusters to upgrade")
		})
	}
}

// --- Best version selection controller ---

type fakeResolver struct {
	version *semver.Version
	err     error
}

func (r fakeResolver) BestExactVersion(_ context.Context, _ string, _ int) (*semver.Version, error) {
	return r.version, r.err
}

func TestBestVersionSelectionSyncOnce(t *testing.T) {
	tests := []struct {
		name        string
		resolver    fakeResolver
		minVersions map[string]semver.Version
		wantBest    string
	}{
		{
			name:     "sets resolved best version",
			resolver: fakeResolver{version: ptrV("4.21.6")},
			wantBest: "4.21.6",
		},
		{
			name:        "floors best version by channel minimum",
			resolver:    fakeResolver{version: ptrV("4.21.2")},
			minVersions: map[string]semver.Version{testChannel: mustV("4.21.5")},
			wantBest:    "4.21.5",
		},
		{
			name:     "no version available is a no-op",
			resolver: fakeResolver{version: nil},
			wantBest: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			mock, err := fleetcosmosstoragetesting.NewMockFleetDBClientWithResources(ctx, []any{newRollout(t, testChannel)})
			require.NoError(t, err)

			cfg := DefaultRolloutConfig()
			if tt.minVersions != nil {
				cfg.MinimumVersions = tt.minVersions
			}
			syncer := &bestVersionSelectionSyncer{
				fleetDBClient: mock,
				resolver:      tt.resolver,
				config:        cfg,
				cooldown:      controllerutil.NewTimeBasedCooldownChecker(time.Minute),
			}

			err = syncer.SyncOnce(ctx, controllerutils.ControlPlaneVersionRolloutKey{Channel: testChannel})
			require.NoError(t, err)

			got, err := mock.ControlPlaneVersionRollouts().Get(ctx, testChannel)
			require.NoError(t, err)
			if tt.wantBest == "" {
				assert.Nil(t, got.Spec.BestExactVersion)
			} else {
				require.NotNil(t, got.Spec.BestExactVersion)
				assert.Equal(t, tt.wantBest, got.Spec.BestExactVersion.String())
			}
		})
	}
}

// --- Cluster service update controller ---

func TestClusterServiceUpdateSyncOnce(t *testing.T) {
	ctx := testContext(t)
	const name = "cluster1"

	mockDB := corecosmosstoragetesting.NewMockResourcesDBClient()
	rid := clusterResourceID(t, name)

	spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockDB, rid)
	require.NoError(t, err)
	spc.Spec.ControlPlaneVersion.DesiredVersion = ptrV("4.21.6")
	spc.Spec.DesiredHostedCluster = &v1beta1.HostedCluster{}
	_, err = mockDB.ServiceProviderClusters(testSubscriptionID, testResourceGroupName, name).Replace(ctx, spc, nil)
	require.NoError(t, err)

	syncer := &clusterServiceUpdateSyncer{
		resourcesDBClient:            mockDB,
		serviceProviderClusterLister: &corelistertesting.DBServiceProviderClusterLister{ResourcesDBClient: mockDB},
	}

	err = syncer.SyncOnce(ctx, controllerutils.HCPClusterKey{
		SubscriptionID:    testSubscriptionID,
		ResourceGroupName: testResourceGroupName,
		HCPClusterName:    name,
	})
	require.NoError(t, err)

	got, err := mockDB.ServiceProviderClusters(testSubscriptionID, testResourceGroupName, name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	require.NotNil(t, got.Spec.DesiredHostedCluster)
	require.NotNil(t, got.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease)
	assert.Equal(t, releaseImage(mustV("4.21.6")), got.Spec.DesiredHostedCluster.Spec.ControlPlaneRelease.Image)
}

// --- Forced desired version assignment controller ---

func TestForcedDesiredVersionAssignmentSyncOnce(t *testing.T) {
	tests := []struct {
		name         string
		pinExact     string
		pinUntil     string
		desired      string
		best         string
		wantDesired  string
		wantPinAfter bool
	}{
		{
			name:         "pin expired adopts best and clears pin",
			pinExact:     "4.21.5",
			pinUntil:     "4.21.6",
			desired:      "4.21.5",
			best:         "4.21.6",
			wantDesired:  "4.21.6",
			wantPinAfter: false,
		},
		{
			name:         "active pin is enforced",
			pinExact:     "4.21.5",
			pinUntil:     "4.21.9",
			desired:      "4.20.1",
			best:         "4.21.6",
			wantDesired:  "4.21.5",
			wantPinAfter: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			const name = "cluster1"
			mockDB := corecosmosstoragetesting.NewMockResourcesDBClient()
			rid := clusterResourceID(t, name)

			cluster := &coreapi.HCPOpenShiftCluster{
				CosmosMetadata: coreapi.CosmosMetadata{
					ResourceID:   rid,
					PartitionKey: strings.ToLower(testSubscriptionID),
				},
				TrackedResource: coreapi.TrackedResource{
					Resource: coreapi.Resource{
						ID:   rid,
						Name: name,
						Type: coreapi.ClusterResourceType.String(),
					},
					Location: "eastus",
				},
				CustomerProperties: coreapi.HCPOpenShiftClusterCustomerProperties{
					Version: coreapi.VersionProfile{ID: testMinor, ChannelGroup: "stable"},
				},
			}
			_, err := mockDB.HCPClusters(testSubscriptionID, testResourceGroupName).Create(ctx, cluster, nil)
			require.NoError(t, err)

			spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockDB, rid)
			require.NoError(t, err)
			spc.Spec.ControlPlaneVersion.DesiredVersion = ptrV(tt.desired)
			withPin(spc, tt.pinExact, tt.pinUntil)
			_, err = mockDB.ServiceProviderClusters(testSubscriptionID, testResourceGroupName, name).Replace(ctx, spc, nil)
			require.NoError(t, err)

			rollout := newRollout(t, testChannel)
			rollout.Spec.BestExactVersion = ptrV(tt.best)

			syncer := &forcedDesiredVersionAssignmentSyncer{
				resourcesDBClient:            mockDB,
				serviceProviderClusterLister: &corelistertesting.DBServiceProviderClusterLister{ResourcesDBClient: mockDB},
				rolloutLister:                &fleetlistertesting.SliceControlPlaneVersionRolloutLister{ControlPlaneVersionRollouts: []*fleetapi.ControlPlaneVersionRollout{rollout}},
			}

			err = syncer.SyncOnce(ctx, controllerutils.HCPClusterKey{
				SubscriptionID:    testSubscriptionID,
				ResourceGroupName: testResourceGroupName,
				HCPClusterName:    name,
			})
			require.NoError(t, err)

			got, err := mockDB.ServiceProviderClusters(testSubscriptionID, testResourceGroupName, name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
			require.NoError(t, err)
			require.NotNil(t, got.Spec.ControlPlaneVersion.DesiredVersion)
			assert.Equal(t, tt.wantDesired, got.Spec.ControlPlaneVersion.DesiredVersion.String())
			if tt.wantPinAfter {
				assert.NotNil(t, got.Spec.PinnedVersion, "pin should remain")
			} else {
				assert.Nil(t, got.Spec.PinnedVersion, "pin should be cleared")
			}
		})
	}
}
