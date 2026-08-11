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
	"fmt"

	"github.com/blang/semver/v4"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/fleetcosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/fleetinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// NormalDesiredVersionAssignmentControllerName is the Cosmos controller document ID for this syncer.
const NormalDesiredVersionAssignmentControllerName = "NormalControlPlaneDesiredVersionAssignment"

// rolloutActionType describes the decision the rollout engine reached.
type rolloutActionType string

const (
	rolloutActionFailed      rolloutActionType = "Failed"
	rolloutActionStable      rolloutActionType = "Stable"
	rolloutActionCanary      rolloutActionType = "Canary"
	rolloutActionWaitCanary  rolloutActionType = "WaitingForCanary"
	rolloutActionRolling     rolloutActionType = "Rolling"
	rolloutActionSteadyState rolloutActionType = "SteadyState"
)

// rolloutPlan is the outcome of planRollout: which clusters to upgrade and the
// condition to publish.
type rolloutPlan struct {
	action           rolloutActionType
	toUpgrade        []*coreapi.ServiceProviderCluster
	conditionStatus  metav1.ConditionStatus
	conditionReason  string
	conditionMessage string
}

// normalDesiredVersionAssignmentSyncer implements the rollout engine: it advances
// eligible clusters toward the channel's best exact version in canary -> rolling
// waves while respecting the failure budget.
type normalDesiredVersionAssignmentSyncer struct {
	fleetDBClient                fleetcosmosstorage.FleetDBClient
	resourcesDBClient            corecosmosstorage.ResourcesDBClient
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
	config                       RolloutConfig
	cooldown                     controllerutil.CooldownChecker
}

var _ controllerutils.ControlPlaneVersionRolloutSyncer = (*normalDesiredVersionAssignmentSyncer)(nil)

// NewNormalDesiredVersionAssignmentController creates the per-rollout controller
// that drives the canary/rolling rollout engine.
func NewNormalDesiredVersionAssignmentController(
	fleetDBClient fleetcosmosstorage.FleetDBClient,
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	fleetInformers fleetinformers.FleetInformers,
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister,
	config RolloutConfig,
) controllerutils.Controller {
	syncer := &normalDesiredVersionAssignmentSyncer{
		fleetDBClient:                fleetDBClient,
		resourcesDBClient:            resourcesDBClient,
		serviceProviderClusterLister: serviceProviderClusterLister,
		config:                       config,
		cooldown:                     controllerutil.NewTimeBasedCooldownChecker(DefaultResyncDuration),
	}
	return controllerutils.NewControlPlaneVersionRolloutWatchingController(
		NormalDesiredVersionAssignmentControllerName,
		fleetDBClient,
		fleetInformers,
		DefaultResyncDuration,
		syncer,
	)
}

func (c *normalDesiredVersionAssignmentSyncer) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldown
}

func (c *normalDesiredVersionAssignmentSyncer) SyncOnce(ctx context.Context, key controllerutils.ControlPlaneVersionRolloutKey) error {
	rollouts := c.fleetDBClient.ControlPlaneVersionRollouts()
	rollout, err := rollouts.Get(ctx, key.Channel)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ControlPlaneVersionRollout %q: %w", key.Channel, err))
	}

	best := rollout.Spec.BestExactVersion
	if best == nil {
		return nil // nothing to roll out to yet
	}
	minor := ChannelMinor(key.Channel)

	spcs, err := c.serviceProviderClusterLister.List(ctx)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to list ServiceProviderClusters: %w", err))
	}

	eligible := eligibleClusters(spcs, minor, *best)
	totalInChannel := countInChannel(spcs, minor)

	plan := planRollout(rollout, eligible, totalInChannel, c.config)

	for _, spc := range plan.toUpgrade {
		if err := c.setDesiredVersion(ctx, spc, best); err != nil {
			return err
		}
	}

	return c.publishCondition(ctx, rollouts, rollout, plan)
}

func (c *normalDesiredVersionAssignmentSyncer) setDesiredVersion(ctx context.Context, spc *coreapi.ServiceProviderCluster, best *semver.Version) error {
	subscriptionID, resourceGroupName, clusterName, ok := clusterCoords(spc)
	if !ok {
		return nil // cannot address the cluster; skip
	}
	if spc.Spec.ControlPlaneVersion.DesiredVersion != nil && spc.Spec.ControlPlaneVersion.DesiredVersion.EQ(*best) {
		return nil
	}
	updated := spc.DeepCopy()
	updated.Spec.ControlPlaneVersion.DesiredVersion = best
	_, err := c.resourcesDBClient.ServiceProviderClusters(subscriptionID, resourceGroupName, clusterName).Replace(ctx, updated, nil)
	return ignoreWriteConflict(err)
}

func (c *normalDesiredVersionAssignmentSyncer) publishCondition(ctx context.Context, rollouts fleetcosmosstorage.ControlPlaneVersionRolloutsCRUD, rollout *fleetapi.ControlPlaneVersionRollout, plan rolloutPlan) error {
	updated := rollout.DeepCopy()
	apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type:    fleetapi.ControlPlaneVersionRolloutConditionProgressing,
		Status:  plan.conditionStatus,
		Reason:  plan.conditionReason,
		Message: plan.conditionMessage,
	})
	if equality.Semantic.DeepEqual(rollout.Status.Conditions, updated.Status.Conditions) {
		return nil
	}
	if _, err := rollouts.Replace(ctx, updated, rollout, nil); err != nil {
		return ignoreWriteConflict(err)
	}
	return nil
}

// planRollout is the pure rollout decision function. best is assumed non-nil
// (rollout.Spec.BestExactVersion). It returns the clusters to upgrade this pass
// and the Progressing condition to publish.
func planRollout(rollout *fleetapi.ControlPlaneVersionRollout, eligible []*coreapi.ServiceProviderCluster, totalInChannel int, cfg RolloutConfig) rolloutPlan {
	best := rollout.Spec.BestExactVersion
	bestStr := best.String()
	status := rollout.Status

	failed := status.FailedClusterCountByDesiredExactVersion[bestStr]
	totalAtBest := status.ClusterCountByDesiredExactVersion[bestStr]

	// Failure budget: halt if more than 2 clusters, or more than 5% of the clusters
	// targeting the best version, have failed.
	if failed > 2 || (totalAtBest > 0 && float64(failed) > 0.05*float64(totalAtBest)) {
		return rolloutPlan{
			action:           rolloutActionFailed,
			conditionStatus:  metav1.ConditionFalse,
			conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonFailed,
			conditionMessage: fmt.Sprintf("rollout halted: %d clusters failed to reach %s", failed, bestStr),
		}
	}

	if len(eligible) == 0 {
		return rolloutPlan{
			action:           rolloutActionStable,
			conditionStatus:  metav1.ConditionFalse,
			conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonStable,
			conditionMessage: fmt.Sprintf("no eligible clusters to roll out to %s", bestStr),
		}
	}

	atBest := status.MismatchedClusterCountByDesiredExactVersion[bestStr] + status.ClusterCountByAchievedExactVersion[bestStr]
	successful := status.SuccessfulClusterCountByAchievedExactVersion[bestStr]

	canaryTarget := percentCeil(cfg.CanaryPercentage, totalInChannel) + 2
	canaryReadyTarget := percentCeil(cfg.CanaryPercentage, totalInChannel)
	rollingTarget := percentCeil(cfg.RollingPercentage, totalInChannel)

	// Canary wave: bring the number of clusters targeting the best version up to
	// the canary target.
	if atBest < canaryTarget {
		selected := selectN(eligible, int(canaryTarget-atBest))
		return rolloutPlan{
			action:           rolloutActionCanary,
			toUpgrade:        selected,
			conditionStatus:  metav1.ConditionTrue,
			conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonProgressing,
			conditionMessage: fmt.Sprintf("selecting %d canary cluster(s) for %s", len(selected), bestStr),
		}
	}

	// Wait for the canaries to become ready before widening the rollout.
	if successful < canaryReadyTarget {
		return rolloutPlan{
			action:           rolloutActionWaitCanary,
			conditionStatus:  metav1.ConditionTrue,
			conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonProgressing,
			conditionMessage: fmt.Sprintf("waiting for canary clusters to become ready at %s", bestStr),
		}
	}

	// Rolling wave: bring the number of clusters targeting the best version up to
	// the rolling target.
	if atBest < rollingTarget {
		selected := selectN(eligible, int(rollingTarget-atBest))
		return rolloutPlan{
			action:           rolloutActionRolling,
			toUpgrade:        selected,
			conditionStatus:  metav1.ConditionTrue,
			conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonProgressing,
			conditionMessage: fmt.Sprintf("rolling out %s to %d cluster(s)", bestStr, len(selected)),
		}
	}

	return rolloutPlan{
		action:           rolloutActionSteadyState,
		conditionStatus:  metav1.ConditionTrue,
		conditionReason:  fleetapi.ControlPlaneVersionRolloutReasonProgressing,
		conditionMessage: fmt.Sprintf("rollout of %s in progress", bestStr),
	}
}

// eligibleClusters returns the clusters that are candidates to be advanced to
// best: their desired version is in the channel's minor and below best, and they
// are either unpinned or pinned with an expiry at or below best.
func eligibleClusters(spcs []*coreapi.ServiceProviderCluster, minor string, best semver.Version) []*coreapi.ServiceProviderCluster {
	var eligible []*coreapi.ServiceProviderCluster
	for _, spc := range spcs {
		desired := spc.Spec.ControlPlaneVersion.DesiredVersion
		if desired == nil || versionMinor(*desired) != minor || !desired.LT(best) {
			continue
		}
		pin := spc.Spec.PinnedVersion
		if pin == nil || pin.ExactVersion == nil {
			eligible = append(eligible, spc)
			continue
		}
		// Pinned: only eligible once the pin's expiry is at or below best.
		if pin.UntilExactVersion != nil && !pin.UntilExactVersion.GT(best) {
			eligible = append(eligible, spc)
		}
	}
	return eligible
}

// countInChannel counts the clusters whose desired version is in the given minor.
func countInChannel(spcs []*coreapi.ServiceProviderCluster, minor string) int {
	count := 0
	for _, spc := range spcs {
		desired := spc.Spec.ControlPlaneVersion.DesiredVersion
		if desired != nil && versionMinor(*desired) == minor {
			count++
		}
	}
	return count
}
