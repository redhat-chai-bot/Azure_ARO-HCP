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

// Package cpversionrollout implements the fleet-level control plane version
// rollout controllers described in
// docs/controllers/fleet-control-plane-version-rollout.md.
package cpversionrollout

import (
	"context"
	"time"

	"github.com/blang/semver/v4"
)

// Defaults for the rollout policy. These are the SRE-configurable
// Code.ControlPlaneUpgradeController.* inputs from the design; until real config
// plumbing exists they are modeled as a RolloutConfig with these defaults.
const (
	// DefaultZStreamOffset selects the version this many z-streams behind the latest.
	DefaultZStreamOffset = 2
	// DefaultCanaryPercentage is the percent of clusters to upgrade first as canaries.
	DefaultCanaryPercentage = 5
	// DefaultRollingPercentage is the percent of clusters to upgrade at the same time.
	DefaultRollingPercentage = 5
	// DefaultMinVersionReadyDuration is the minimum time a control plane must be at
	// the desired version before it is considered successful.
	DefaultMinVersionReadyDuration = 30 * time.Minute
	// DefaultMaxUpgradeDuration is the maximum time to wait for a control plane
	// upgrade before treating the cluster as failed.
	DefaultMaxUpgradeDuration = 2 * time.Hour
	// DefaultResyncDuration is the interval at which per-rollout controllers re-run.
	DefaultResyncDuration = 5 * time.Minute
)

// RolloutConfig captures the SRE-specified rollout policy inputs. See the design
// doc's Code.ControlPlaneUpgradeController.* fields.
type RolloutConfig struct {
	// ZStreamOffset is the number of z-streams behind the latest to select.
	ZStreamOffset int
	// CanaryPercentage is the percent of clusters to upgrade first as canaries.
	CanaryPercentage int
	// RollingPercentage is the percent of clusters to upgrade at the same time.
	RollingPercentage int
	// MinVersionReadyDuration is the minimum time at the desired version before success.
	MinVersionReadyDuration time.Duration
	// MaxUpgradeDuration is the maximum time to wait for an upgrade before failing.
	MaxUpgradeDuration time.Duration
	// MinimumVersions is the per-channel SRE-specified minimum allowed exact version.
	MinimumVersions map[string]semver.Version
}

// DefaultRolloutConfig returns the default rollout policy.
func DefaultRolloutConfig() RolloutConfig {
	return RolloutConfig{
		ZStreamOffset:           DefaultZStreamOffset,
		CanaryPercentage:        DefaultCanaryPercentage,
		RollingPercentage:       DefaultRollingPercentage,
		MinVersionReadyDuration: DefaultMinVersionReadyDuration,
		MaxUpgradeDuration:      DefaultMaxUpgradeDuration,
		MinimumVersions:         map[string]semver.Version{},
	}
}

// BestVersionResolver resolves the newest risk-free z-stream for a channel,
// offset by zStreamOffset from the latest available z-stream. It abstracts the
// Cincinnati upgrade-graph query so the best-version-selection controller can be
// unit tested. The production implementation is backed by the Cincinnati graph
// client (a follow-up).
type BestVersionResolver interface {
	BestExactVersion(ctx context.Context, channel string, zStreamOffset int) (*semver.Version, error)
}

// NoopBestVersionResolver is a placeholder resolver that reports no available
// version. It keeps the best-version-selection controller a safe no-op until the
// Cincinnati-backed resolver is wired in (see the plan doc's open questions).
type NoopBestVersionResolver struct{}

var _ BestVersionResolver = NoopBestVersionResolver{}

func (NoopBestVersionResolver) BestExactVersion(_ context.Context, _ string, _ int) (*semver.Version, error) {
	return nil, nil
}
