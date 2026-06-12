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

package ocm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// updateDispatchClusterUpdatableConfig is the set of properties that are updatable by RP Backend's cluster service
// cluster update dispatch controller, against Cluster Service.
// The cluster update dispatch controller compares desired and actual configs and
// sends a CS PATCH only when they differ.
//
// Note: This does not necessarily include all the fields that can be updated via the CS API, just the ones
// that are considered during an ARM Cluster update call and processed by RP Backend's cluster service
// cluster update dispatch controller.
//
// Do not embed internal/api struct types (for example api.ClusterAutoscalingProfile,
// api.ImageDigestMirror, or api.ExperimentalFeatures) in this struct or its nested field types.
// Those api structs evolve for Cosmos persistence, admission, and new API versions. If a field used
// an api struct directly, any new field added to that struct would be marshaled into the config hash
// and treated as updatable automatically, even when the cluster update dispatch controller does not
// read or apply it. Instead, define curated local structs with only the fields that dispatch should
// hash and sync, and copy values explicitly from api types at the conversion boundaries.
//
// Using api enum or scalar types for individual curated fields is fine (for example
// api.ControlPlaneAvailability). Adding a new enum constant does not add a new config field; only
// adding a new field to the curated local struct would.
type updateDispatchClusterUpdatableConfig struct {
	NodeDrainTimeoutMinutes     int32                                                    `json:"nodeDrainTimeoutMinutes,omitempty"`
	K8sAPIServerAuthorizedCIDRs []string                                                 `json:"k8sAPIServerAuthorizedCIDRs,omitempty"`
	ImageDigestMirrors          []updateDispatchClusterUpdatableConfigImageDigestMirror  `json:"imageDigestMirrors,omitempty"`
	Autoscaling                 updateDispatchClusterUpdatableConfigAutoscaling          `json:"autoscaling,omitzero"`
	ExperimentalFeatures        updateDispatchClusterUpdatableConfigExperimentalFeatures `json:"experimentalFeatures,omitzero"`
}

// updateDispatchClusterUpdatableConfigImageDigestMirror is the curated image mirror subset hashed
// and applied to CS. See updateDispatchClusterUpdatableConfig: do not embed api.ImageDigestMirror.
type updateDispatchClusterUpdatableConfigImageDigestMirror struct {
	Source  string   `json:"source,omitempty"`
	Mirrors []string `json:"mirrors,omitempty"`
}

// updateDispatchClusterUpdatableConfigAutoscaling is the curated autoscaling subset hashed
// and applied to CS. See updateDispatchClusterUpdatableConfig: do not embed api.ClusterAutoscalingProfile.
type updateDispatchClusterUpdatableConfigAutoscaling struct {
	MaxNodesTotal               int32 `json:"maxNodesTotal,omitempty"`
	MaxPodGracePeriodSeconds    int32 `json:"maxPodGracePeriodSeconds,omitempty"`
	MaxNodeProvisionTimeSeconds int32 `json:"maxNodeProvisionTimeSeconds,omitempty"`
	PodPriorityThreshold        int32 `json:"podPriorityThreshold,omitempty"`
}

// updateDispatchClusterUpdatableConfigExperimentalFeatures is the curated experimental subset hashed
// and applied to CS. See updateDispatchClusterUpdatableConfig: do not embed api.ExperimentalFeatures.
// Individual api enum fields (ControlPlaneAvailability, ControlPlanePodSizing) are intentional.
type updateDispatchClusterUpdatableConfigExperimentalFeatures struct {
	ControlPlaneAvailability api.ControlPlaneAvailability `json:"singleReplica,omitempty"`
	ControlPlanePodSizing    api.ControlPlanePodSizing    `json:"sizeOverride,omitempty"`
}

// UpdateDispatchClusterUpdatableConfigFromCluster extracts the canonical updatable cluster
// configuration from the cluster's customer and service provider properties.
func UpdateDispatchClusterUpdatableConfigFromCluster(cluster *api.HCPOpenShiftCluster) *updateDispatchClusterUpdatableConfig {
	return &updateDispatchClusterUpdatableConfig{
		NodeDrainTimeoutMinutes:     cluster.CustomerProperties.NodeDrainTimeoutMinutes,
		K8sAPIServerAuthorizedCIDRs: cluster.CustomerProperties.API.AuthorizedCIDRs,
		ImageDigestMirrors:          imageDigestMirrorsFromAPI(cluster.CustomerProperties.ImageDigestMirrors),
		Autoscaling:                 autoscalingFromAPI(cluster.CustomerProperties.Autoscaling),
		ExperimentalFeatures: updateDispatchClusterUpdatableConfigExperimentalFeatures{
			ControlPlaneAvailability: cluster.ServiceProviderProperties.ExperimentalFeatures.ControlPlaneAvailability,
			ControlPlanePodSizing:    cluster.ServiceProviderProperties.ExperimentalFeatures.ControlPlanePodSizing,
		},
	}
}

// UpdateDispatchClusterUpdatableConfigFromClusterServiceCluster extracts the canonical updatable
// cluster configuration from a Cluster Service cluster object.
func UpdateDispatchClusterUpdatableConfigFromClusterServiceCluster(csCluster *arohcpv1alpha1.Cluster) (*updateDispatchClusterUpdatableConfig, error) {
	config := &updateDispatchClusterUpdatableConfig{}

	if nodeDrainGracePeriod := csCluster.NodeDrainGracePeriod(); nodeDrainGracePeriod != nil {
		value, ok := nodeDrainGracePeriod.GetValue()
		if !ok {
			return nil, utils.TrackError(fmt.Errorf("node drain grace period value is missing"))
		}
		config.NodeDrainTimeoutMinutes = int32(value)
	}

	if clusterAPI := csCluster.API(); clusterAPI != nil {
		authorizedCIDRs, err := authorizedCIDRsFromClusterServiceAPI(clusterAPI)
		if err != nil {
			return nil, err
		}
		config.K8sAPIServerAuthorizedCIDRs = authorizedCIDRs
	}

	if registryConfig := csCluster.RegistryConfig(); registryConfig != nil {
		imageDigestMirrors, ok := registryConfig.GetImageDigestMirrors()
		if ok && len(imageDigestMirrors) > 0 {
			config.ImageDigestMirrors = make([]updateDispatchClusterUpdatableConfigImageDigestMirror, 0, len(imageDigestMirrors))
			for _, mirror := range imageDigestMirrors {
				source, sourceOK := mirror.GetSource()
				mirrors, mirrorsOK := mirror.GetMirrors()
				if !sourceOK {
					continue
				}
				item := updateDispatchClusterUpdatableConfigImageDigestMirror{Source: source}
				if mirrorsOK {
					item.Mirrors = append([]string(nil), mirrors...)
				}
				config.ImageDigestMirrors = append(config.ImageDigestMirrors, item)
			}
		}
	}

	for key, value := range csCluster.Properties() {
		switch key {
		case CSPropertySingleReplica:
			if value == CSPropertyEnabled {
				config.ExperimentalFeatures.ControlPlaneAvailability = api.SingleReplicaControlPlane
			}
		case CSPropertySizeOverride:
			if value == CSPropertyEnabled {
				config.ExperimentalFeatures.ControlPlanePodSizing = api.MinimalControlPlanePodSizing
			}
		}
	}

	if autoscaler := csCluster.Autoscaler(); autoscaler != nil {
		autoscaling, err := convertCSAutoscalerToUpdatableConfig(autoscaler)
		if err != nil {
			return nil, err
		}
		config.Autoscaling = autoscaling
	}

	return config, nil
}

// UpdateDispatchClusterUpdatableConfigDiffersFromClusterService reports whether the updatable
// configuration derived from the RP cluster differs from the live Cluster Service cluster.
func UpdateDispatchClusterUpdatableConfigDiffersFromClusterService(cluster *api.HCPOpenShiftCluster, csCluster *arohcpv1alpha1.Cluster) (bool, error) {
	desiredHash, err := updateDispatchClusterUpdatableConfigHash(UpdateDispatchClusterUpdatableConfigFromCluster(cluster))
	if err != nil {
		return false, err
	}

	actualConfig, err := UpdateDispatchClusterUpdatableConfigFromClusterServiceCluster(csCluster)
	if err != nil {
		return false, err
	}
	actualHash, err := updateDispatchClusterUpdatableConfigHash(actualConfig)
	if err != nil {
		return false, err
	}

	return desiredHash != actualHash, nil
}

// UpdateDispatchClusterUpdatableConfigHash returns a SHA-256 hex digest of
// clusterUpdatableConfig built from the cluster properties marshaled as a json map.
func UpdateDispatchClusterUpdatableConfigHash(cluster *api.HCPOpenShiftCluster) (string, error) {
	return updateDispatchClusterUpdatableConfigHash(UpdateDispatchClusterUpdatableConfigFromCluster(cluster))
}

func updateDispatchClusterUpdatableConfigHash(config *updateDispatchClusterUpdatableConfig) (string, error) {
	raw, err := updateDispatchClusterUpdatableConfigJSONForHash(config)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func authorizedCIDRsFromClusterServiceAPI(clusterAPI *arohcpv1alpha1.ClusterAPI) ([]string, error) {
	cidrBlockAccess, ok := clusterAPI.GetCIDRBlockAccess()
	if !ok || cidrBlockAccess == nil {
		return nil, nil
	}

	allow, ok := cidrBlockAccess.GetAllow()
	if !ok || allow == nil {
		return nil, nil
	}

	mode, ok := allow.GetMode()
	if !ok {
		return nil, nil
	}

	switch mode {
	case csCIDRBlockAllowAccessModeAllowAll:
		return nil, nil
	case csCIDRBlockAllowAccessModeAllowList:
		values, ok := allow.GetValues()
		if !ok {
			return nil, utils.TrackError(fmt.Errorf("CIDR block allow list mode is missing values"))
		}
		return append([]string(nil), values...), nil
	default:
		return nil, utils.TrackError(fmt.Errorf("unknown CIDR block allow access mode %q", mode))
	}
}

func imageDigestMirrorsFromAPI(mirrors []api.ImageDigestMirror) []updateDispatchClusterUpdatableConfigImageDigestMirror {
	if len(mirrors) == 0 {
		return nil
	}

	out := make([]updateDispatchClusterUpdatableConfigImageDigestMirror, 0, len(mirrors))
	for _, mirror := range mirrors {
		out = append(out, updateDispatchClusterUpdatableConfigImageDigestMirror{
			Source:  mirror.Source,
			Mirrors: append([]string(nil), mirror.Mirrors...),
		})
	}
	return out
}

func imageDigestMirrorsToAPI(mirrors []updateDispatchClusterUpdatableConfigImageDigestMirror) []api.ImageDigestMirror {
	if len(mirrors) == 0 {
		return nil
	}

	out := make([]api.ImageDigestMirror, 0, len(mirrors))
	for _, mirror := range mirrors {
		out = append(out, api.ImageDigestMirror{
			Source:  mirror.Source,
			Mirrors: append([]string(nil), mirror.Mirrors...),
		})
	}
	return out
}

func autoscalingFromAPI(profile api.ClusterAutoscalingProfile) updateDispatchClusterUpdatableConfigAutoscaling {
	return updateDispatchClusterUpdatableConfigAutoscaling{
		MaxNodesTotal:               profile.MaxNodesTotal,
		MaxPodGracePeriodSeconds:    profile.MaxPodGracePeriodSeconds,
		MaxNodeProvisionTimeSeconds: profile.MaxNodeProvisionTimeSeconds,
		PodPriorityThreshold:        profile.PodPriorityThreshold,
	}
}

func autoscalingToAPI(profile updateDispatchClusterUpdatableConfigAutoscaling) api.ClusterAutoscalingProfile {
	return api.ClusterAutoscalingProfile{
		MaxNodesTotal:               profile.MaxNodesTotal,
		MaxPodGracePeriodSeconds:    profile.MaxPodGracePeriodSeconds,
		MaxNodeProvisionTimeSeconds: profile.MaxNodeProvisionTimeSeconds,
		PodPriorityThreshold:        profile.PodPriorityThreshold,
	}
}

func convertCSAutoscalerToUpdatableConfig(autoscaler *arohcpv1alpha1.ClusterAutoscaler) (updateDispatchClusterUpdatableConfigAutoscaling, error) {
	profile := updateDispatchClusterUpdatableConfigAutoscaling{}

	if maxNodeProvisionTime, ok := autoscaler.GetMaxNodeProvisionTime(); ok && maxNodeProvisionTime != "" {
		duration, err := time.ParseDuration(maxNodeProvisionTime)
		if err != nil {
			return profile, utils.TrackError(fmt.Errorf("failed to parse max node provision time %q: %w", maxNodeProvisionTime, err))
		}
		profile.MaxNodeProvisionTimeSeconds = int32(duration.Seconds())
	}

	if maxPodGracePeriod, ok := autoscaler.GetMaxPodGracePeriod(); ok {
		profile.MaxPodGracePeriodSeconds = int32(maxPodGracePeriod)
	}

	if podPriorityThreshold, ok := autoscaler.GetPodPriorityThreshold(); ok {
		profile.PodPriorityThreshold = int32(podPriorityThreshold)
	}

	if resourceLimits, ok := autoscaler.GetResourceLimits(); ok && resourceLimits != nil {
		if maxNodesTotal, ok := resourceLimits.GetMaxNodesTotal(); ok {
			profile.MaxNodesTotal = int32(maxNodesTotal)
		}
	}

	return profile, nil
}

// updateDispatchClusterUpdatableConfigJSONForHash returns canonical JSON for hashing. The struct
// is marshaled first so json tags and omitempty apply, then round-tripped through
// map[string]any so object keys are emitted in sorted order at every level.
func updateDispatchClusterUpdatableConfigJSONForHash(config *updateDispatchClusterUpdatableConfig) ([]byte, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to marshal cluster updatable config: %w", err))
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to unmarshal cluster updatable config: %w", err))
	}

	raw, err = json.Marshal(payload)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to marshal cluster updatable config payload: %w", err))
	}
	return raw, nil
}

// applyUpdateDispatchClusterUpdatableConfig applies clusterUpdatableConfig to Cluster Service.
// baseProperties may be nil or contain arbitrary existing entries (for example base
// layers from old CS properties and caller requiredProperties). This function
// overlays updatable experimental feature flags onto that map and registers it
// on the builder. Keys are set to CSPropertyEnabled when enabled and deleted
// when disabled so tag removal clears previously set values.
func applyUpdateDispatchClusterUpdatableConfig(clusterBuilder *arohcpv1alpha1.ClusterBuilder, clusterAPIBuilder *arohcpv1alpha1.ClusterAPIBuilder, baseProperties map[string]string, config *updateDispatchClusterUpdatableConfig) error {
	if baseProperties == nil {
		baseProperties = map[string]string{}
	}

	clusterBuilder.NodeDrainGracePeriod(arohcpv1alpha1.NewValue().
		Unit(csNodeDrainGracePeriodUnit).
		Value(float64(config.NodeDrainTimeoutMinutes)))

	cidrBlockAccess, err := convertCIDRBlockAllowAccessRPToCS(api.CustomerAPIProfile{
		AuthorizedCIDRs: config.K8sAPIServerAuthorizedCIDRs,
	})
	if err != nil {
		return err
	}
	clusterBuilder.API(clusterAPIBuilder.CIDRBlockAccess(cidrBlockAccess))

	clusterBuilder.RegistryConfig(arohcpv1alpha1.NewClusterRegistryConfig().
		ImageDigestMirrors(convertImageDigestMirrorsToCSBuilder(imageDigestMirrorsToAPI(config.ImageDigestMirrors))...))

	experimentalFeatures := config.ExperimentalFeatures
	if experimentalFeatures.ControlPlaneAvailability == api.SingleReplicaControlPlane {
		baseProperties[CSPropertySingleReplica] = CSPropertyEnabled
	} else {
		delete(baseProperties, CSPropertySingleReplica)
	}
	if experimentalFeatures.ControlPlanePodSizing == api.MinimalControlPlanePodSizing {
		baseProperties[CSPropertySizeOverride] = CSPropertyEnabled
	} else {
		delete(baseProperties, CSPropertySizeOverride)
	}
	clusterBuilder.Properties(baseProperties)

	return nil
}

func applyUpdateDispatchClusterUpdatableAutoscalerConfig(config *updateDispatchClusterUpdatableConfig) (*arohcpv1alpha1.ClusterAutoscalerBuilder, error) {
	profile := autoscalingToAPI(config.Autoscaling)
	return convertRpAutoscalarToCSBuilder(&profile)
}
