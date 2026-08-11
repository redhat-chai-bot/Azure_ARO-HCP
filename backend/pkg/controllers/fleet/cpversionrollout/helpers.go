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
	"fmt"
	"sort"
	"strings"

	"github.com/blang/semver/v4"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
)

// ChannelMinor extracts the "x.y" minor version from a y-stream channel name such
// as "stable-4.21" (-> "4.21"). Returns "" when the channel has no suffix.
func ChannelMinor(channel string) string {
	idx := strings.LastIndex(channel, "-")
	if idx < 0 || idx+1 >= len(channel) {
		return ""
	}
	return channel[idx+1:]
}

// versionMinor returns the "x.y" minor of a semantic version.
func versionMinor(v semver.Version) string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// currentActiveVersion returns the most recent active version of a cluster's
// control plane (activeVersions is ordered most-recent-first). Returns nil when
// there are no active versions.
func currentActiveVersion(spc *coreapi.ServiceProviderCluster) *semver.Version {
	versions := spc.Status.ControlPlaneVersion.ActiveVersions
	if len(versions) == 0 {
		return nil
	}
	return versions[0].Version
}

// clusterAchievedDesired reports whether the cluster's current (most recent)
// active version equals its desired version.
func clusterAchievedDesired(spc *coreapi.ServiceProviderCluster) bool {
	desired := spc.Spec.ControlPlaneVersion.DesiredVersion
	if desired == nil {
		return false
	}
	current := currentActiveVersion(spc)
	return current != nil && current.EQ(*desired)
}

// isDegraded reports whether the cluster has a Degraded=True condition, used as a
// proxy for "this cluster's upgrade is failing".
func isDegraded(spc *coreapi.ServiceProviderCluster) bool {
	return apimeta.IsStatusConditionTrue(spc.Status.Conditions, coreapi.DegradedCondition)
}

// clusterCoords extracts the (subscription, resourceGroup, clusterName) tuple
// needed to address the ServiceProviderCluster's CRUD from its resource ID.
// The SPC resource ID is nested directly under its HCP cluster.
func clusterCoords(spc *coreapi.ServiceProviderCluster) (subscriptionID, resourceGroupName, clusterName string, ok bool) {
	rid := spc.CosmosMetadata.ResourceID
	if rid == nil || rid.Parent == nil {
		return "", "", "", false
	}
	if rid.SubscriptionID == "" || rid.ResourceGroupName == "" || rid.Parent.Name == "" {
		return "", "", "", false
	}
	return rid.SubscriptionID, rid.ResourceGroupName, rid.Parent.Name, true
}

// sortSPCsByResourceID sorts clusters deterministically by their resource ID.
// The design specifies "random" canary/rolling selection; we use deterministic
// name-sorted selection so the behavior is reviewable and testable, leaving a
// seam to swap in richer criteria later.
func sortSPCsByResourceID(spcs []*coreapi.ServiceProviderCluster) {
	sort.Slice(spcs, func(i, j int) bool {
		return spcResourceIDString(spcs[i]) < spcResourceIDString(spcs[j])
	})
}

func spcResourceIDString(spc *coreapi.ServiceProviderCluster) string {
	if spc.CosmosMetadata.ResourceID == nil {
		return ""
	}
	return strings.ToLower(spc.CosmosMetadata.ResourceID.String())
}

// selectN returns the first n clusters after deterministic sorting. n is clamped
// to [0, len(eligible)].
func selectN(eligible []*coreapi.ServiceProviderCluster, n int) []*coreapi.ServiceProviderCluster {
	if n <= 0 {
		return nil
	}
	sorted := make([]*coreapi.ServiceProviderCluster, len(eligible))
	copy(sorted, eligible)
	sortSPCsByResourceID(sorted)
	if n > len(sorted) {
		n = len(sorted)
	}
	return sorted[:n]
}

// percentCeil returns ceil(pct% of total) as an int64.
func percentCeil(pct int, total int) int64 {
	if pct <= 0 || total <= 0 {
		return 0
	}
	return int64((pct*total + 99) / 100)
}

// ignoreWriteConflict treats optimistic-concurrency and not-found errors as
// no-ops: the next reconcile re-establishes the desired state.
func ignoreWriteConflict(err error) error {
	if err == nil {
		return nil
	}
	if cosmosstorageutils.IsPreconditionFailedError(err) || cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	return err
}
