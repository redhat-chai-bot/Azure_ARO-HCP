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

package fleetapi

import (
	"github.com/blang/semver/v4"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
)

// ControlPlaneVersionRolloutConditionType represents the type of a control plane
// version rollout condition.
type ControlPlaneVersionRolloutConditionType = string

const (
	// ControlPlaneVersionRolloutConditionProgressing is True while the rollout is
	// actively advancing clusters toward Spec.BestExactVersion, False when the
	// rollout is stable (no eligible clusters), and its Reason encodes failure.
	ControlPlaneVersionRolloutConditionProgressing ControlPlaneVersionRolloutConditionType = "Progressing"
)

// Known ControlPlaneVersionRollout Progressing condition Reason values.
const (
	// ControlPlaneVersionRolloutReasonStable indicates there are no eligible
	// clusters left to roll out; the fleet is at the desired version for the channel.
	ControlPlaneVersionRolloutReasonStable = "Stable"
	// ControlPlaneVersionRolloutReasonProgressing indicates the rollout is in
	// progress (canary or rolling waves are advancing).
	ControlPlaneVersionRolloutReasonProgressing = "Progressing"
	// ControlPlaneVersionRolloutReasonFailed indicates the rollout has exceeded
	// its failure budget for the best exact version and has halted.
	ControlPlaneVersionRolloutReasonFailed = "Failed"
)

// ControlPlaneVersionRollout is a region-wide record of the version rollout state
// for a single y-stream channel. The document name is the y-stream channel it is
// associated with (e.g. "stable-4.21").
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ControlPlaneVersionRollout struct {
	// CosmosMetadata ResourceID is the ControlPlaneVersionRollout resource ID.
	// PartitionKey holds the lowercased y-stream channel name (this document's name).
	coreapi.CosmosMetadata `json:"cosmosMetadata"`

	// ResourceID exists to match cosmosMetadata.resourceID until we're able to transition all
	// types to use cosmosMetadata, at which point we will stop using properties.resourceId in
	// our queries.
	// Example: "/providers/microsoft.redhatopenshift/controlplaneversionrollouts/stable-4.21"
	//
	// +required, immutable once set.
	ResourceID *azcorearm.ResourceID `json:"resourceId,omitempty"`

	// Spec contains the desired rollout target for the channel.
	Spec ControlPlaneVersionRolloutSpec `json:"spec"`

	// Status contains the observed rollout state for the channel.
	Status ControlPlaneVersionRolloutStatus `json:"status"`
}

// ControlPlaneVersionRolloutSpec contains the desired rollout target.
type ControlPlaneVersionRolloutSpec struct {
	// BestExactVersion is the most recent z-stream without a platform+controlplane
	// risk in the y-stream channel, offset by zStreamOffset from the latest.
	// Written by: ControlPlaneVersionBestVersionSelection
	BestExactVersion *semver.Version `json:"bestExactVersion,omitempty"`
}

// ControlPlaneVersionRolloutStatus contains the observed rollout state, computed
// across all clusters in the channel.
type ControlPlaneVersionRolloutStatus struct {
	// Conditions tracks the rollout's progression (Progressing).
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ClusterCountByDesiredExactVersion maps a desired exact version to the number
	// of clusters that have that exact version as their
	// ServiceProviderCluster.spec.control_plane_version.desired_version.
	// Written by: ControlPlaneVersionStatusCollector
	ClusterCountByDesiredExactVersion map[string]int64 `json:"clusterCountByDesiredExactVersion,omitempty"`

	// MismatchedClusterCountByDesiredExactVersion maps a desired exact version to
	// the number of clusters that do not have that exact version as the earliest
	// version in their activeVersions.
	// Written by: ControlPlaneVersionStatusCollector
	MismatchedClusterCountByDesiredExactVersion map[string]int64 `json:"mismatchedClusterCountByDesiredExactVersion,omitempty"`

	// FailedClusterCountByDesiredExactVersion maps a desired exact version to the
	// number of clusters that have been mismatched for more than the
	// maxUpgradeDuration for the desired minor version.
	// Written by: ControlPlaneVersionStatusCollector
	FailedClusterCountByDesiredExactVersion map[string]int64 `json:"failedClusterCountByDesiredExactVersion,omitempty"`

	// ClusterCountByAchievedExactVersion maps a desired exact version to the number
	// of clusters that do have that exact version as the earliest version in their
	// activeVersions.
	// Written by: ControlPlaneVersionStatusCollector
	ClusterCountByAchievedExactVersion map[string]int64 `json:"clusterCountByAchievedExactVersion,omitempty"`

	// SuccessfulClusterCountByAchievedExactVersion maps a desired exact version to
	// the number of clusters that have that exact version as the earliest version in
	// their activeVersions and have been at that level for more than
	// minVersionReadyDuration.
	// Written by: ControlPlaneVersionStatusCollector
	SuccessfulClusterCountByAchievedExactVersion map[string]int64 `json:"successfulClusterCountByAchievedExactVersion,omitempty"`
}
