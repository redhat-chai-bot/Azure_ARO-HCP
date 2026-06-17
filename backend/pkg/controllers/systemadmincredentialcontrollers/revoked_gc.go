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

// Package systemadmincredentialcontrollers contains controllers for lifecycle
// management of SystemAdminCredential Cosmos documents that are not
// operation-driven (i.e. not wired through GenericOperationController).
package systemadmincredentialcontrollers

import (
	"context"
	"fmt"
	"time"

	utilsclock "k8s.io/utils/clock"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	// revokedRetentionPeriod is how long a Revoked credential is kept
	// after its RevokedAt timestamp before GC deletes it. The 48h
	// value is chosen to outlast the certificate's 24h TTL, so a stale
	// row can never describe a still-valid kubeconfig.
	revokedRetentionPeriod = 48 * time.Hour
)

// RevokedGC garbage-collects SystemAdminCredential documents whose Phase is
// Revoked and whose RevokedAt + 48h has passed. It is designed to be called
// periodically per cluster (e.g. from a ClusterWatchingController or a
// periodic sweep goroutine).
//
// The struct is exported so callers can construct and unit-test it directly;
// the wiring into a periodic controller framework is left to the caller.
type RevokedGC struct {
	Clock             utilsclock.PassiveClock
	ResourcesDBClient database.ResourcesDBClient
}

// NewRevokedGC constructs a RevokedGC with the given clock and DB client.
func NewRevokedGC(
	clock utilsclock.PassiveClock,
	resourcesDBClient database.ResourcesDBClient,
) *RevokedGC {
	return &RevokedGC{
		Clock:             clock,
		ResourcesDBClient: resourcesDBClient,
	}
}

// SyncCluster scans SystemAdminCredentials under the given cluster and deletes
// any that have been in Revoked state for longer than the retention period.
// It returns the number of credentials deleted and the first error encountered
// (after attempting all eligible deletions).
func (gc *RevokedGC) SyncCluster(
	ctx context.Context,
	subscriptionID, resourceGroupName, clusterName string,
) (int, error) {
	logger := utils.LoggerFromContext(ctx)

	credCRUD := gc.ResourcesDBClient.HCPClusters(
		subscriptionID, resourceGroupName,
	).SystemAdminCredentials(clusterName)

	iter, err := credCRUD.List(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("listing SystemAdminCredentials: %w", err)
	}

	now := gc.Clock.Now()
	var deleted int
	var firstErr error

	for _, cred := range iter.Items(ctx) {
		if cred.Status.Phase != api.SystemAdminCredentialPhaseRevoked {
			continue
		}
		if cred.Status.RevokedAt == nil {
			// Defensive: a Revoked credential without RevokedAt is
			// malformed. Skip rather than delete — an operator should
			// investigate.
			logger.Info("skipping Revoked credential with nil RevokedAt",
				"credential", cred.CosmosMetadata.ResourceID.Name)
			continue
		}
		cutoff := cred.Status.RevokedAt.Time.Add(revokedRetentionPeriod)
		if now.Before(cutoff) {
			continue
		}

		logger.Info("deleting expired Revoked credential",
			"credential", cred.CosmosMetadata.ResourceID.Name,
			"revoked_at", cred.Status.RevokedAt.Time)

		if delErr := credCRUD.Delete(ctx, cred.CosmosMetadata.ResourceID.Name); delErr != nil {
			logger.Error(delErr, "failed to delete credential",
				"credential", cred.CosmosMetadata.ResourceID.Name)
			if firstErr == nil {
				firstErr = delErr
			}
			continue
		}
		deleted++
	}
	if err := iter.GetError(); err != nil {
		return deleted, fmt.Errorf("iterating SystemAdminCredentials: %w", err)
	}

	return deleted, firstErr
}
