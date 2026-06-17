// Copyright 2025 Microsoft Corporation
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
	"time"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
)

// defaultCooldown is the resync cooldown for system-admin-credential
// controllers. 5 minutes is a reasonable default — these controllers are
// not latency-critical and their inputs (credential phases, ReadDesire
// status) change infrequently.
const defaultCooldown = 5 * time.Minute

// ---------------------------------------------------------------------------
// Interface assertions
// ---------------------------------------------------------------------------

var (
	_ controllerutils.ClusterSyncer = (*DesiresCreator)(nil)
	_ controllerutils.ClusterSyncer = (*IssuanceObserver)(nil)
	_ controllerutils.ClusterSyncer = (*PostIssuanceCleanup)(nil)
	_ controllerutils.ClusterSyncer = (*ClusterDeletionCleanup)(nil)
	_ controllerutils.ClusterSyncer = (*ServingCAReadDesireCreator)(nil)
	_ controllerutils.ClusterSyncer = (*CABundleSync)(nil)
	_ controllerutils.ClusterSyncer = (*RevokedGC)(nil)
)

// ---------------------------------------------------------------------------
// DesiresCreator
// ---------------------------------------------------------------------------

func (dc *DesiresCreator) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := dc.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (dc *DesiresCreator) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// IssuanceObserver
// ---------------------------------------------------------------------------

func (o *IssuanceObserver) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := o.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (o *IssuanceObserver) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// PostIssuanceCleanup
// ---------------------------------------------------------------------------

func (pc *PostIssuanceCleanup) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := pc.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (pc *PostIssuanceCleanup) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// ClusterDeletionCleanup
// ---------------------------------------------------------------------------

func (cc *ClusterDeletionCleanup) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := cc.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (cc *ClusterDeletionCleanup) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// ServingCAReadDesireCreator
// ---------------------------------------------------------------------------

func (sc *ServingCAReadDesireCreator) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := sc.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (sc *ServingCAReadDesireCreator) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// CABundleSync
// ---------------------------------------------------------------------------

func (cb *CABundleSync) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := cb.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (cb *CABundleSync) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}

// ---------------------------------------------------------------------------
// RevokedGC
// ---------------------------------------------------------------------------

func (gc *RevokedGC) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	_, err := gc.SyncCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	return err
}

func (gc *RevokedGC) CooldownChecker() controllerutil.CooldownChecker {
	return controllerutil.NewTimeBasedCooldownChecker(defaultCooldown)
}
