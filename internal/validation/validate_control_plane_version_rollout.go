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

package validation

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
)

// ValidateControlPlaneVersionRolloutCreate validates a ControlPlaneVersionRollout on create.
func ValidateControlPlaneVersionRolloutCreate(_ context.Context, rollout *fleetapi.ControlPlaneVersionRollout) field.ErrorList {
	return validateControlPlaneVersionRollout(rollout)
}

// ValidateControlPlaneVersionRolloutUpdate validates a ControlPlaneVersionRollout on update.
func ValidateControlPlaneVersionRolloutUpdate(_ context.Context, newRollout *fleetapi.ControlPlaneVersionRollout, _ *fleetapi.ControlPlaneVersionRollout) field.ErrorList {
	return validateControlPlaneVersionRollout(newRollout)
}

func validateControlPlaneVersionRollout(rollout *fleetapi.ControlPlaneVersionRollout) field.ErrorList {
	var errs field.ErrorList
	if len(rollout.GetChannel()) == 0 {
		errs = append(errs, field.Required(
			field.NewPath("cosmosMetadata", "resourceID"),
			"control plane version rollout must have a non-empty channel name",
		))
	}
	return errs
}
