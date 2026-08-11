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

package controllerutils

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/fleetcosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/fleetinformers"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// ControlPlaneVersionRolloutKey identifies a single ControlPlaneVersionRollout by
// its y-stream channel.
type ControlPlaneVersionRolloutKey struct {
	Channel string `json:"channel"`
}

func (k ControlPlaneVersionRolloutKey) GetResourceID() *azcorearm.ResourceID {
	return metadataapi.Must(fleetapi.ToControlPlaneVersionRolloutResourceID(k.Channel))
}

func (k ControlPlaneVersionRolloutKey) AddLoggerValues(logger logr.Logger) logr.Logger {
	return logger.WithValues(
		utils.LogValues{}.
			AddLogValuesForResourceID(k.GetResourceID())...)
}

func (k ControlPlaneVersionRolloutKey) InitialController(controllerName string) *coreapi.Controller {
	resourceID := metadataapi.Must(azcorearm.ParseResourceID(k.GetResourceID().String() + "/" + fleetapi.ControllerResourceTypeName + "/" + controllerName))
	return &coreapi.Controller{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(k.Channel),
		},
		ExternalID: k.GetResourceID(),
		Status: coreapi.ControllerStatus{
			Conditions: []metav1.Condition{},
		},
	}
}

type ControlPlaneVersionRolloutSyncer interface {
	SyncOnce(ctx context.Context, key ControlPlaneVersionRolloutKey) error
	CooldownChecker() controllerutil.CooldownChecker
}

type controlPlaneVersionRolloutWatchingController struct {
	name          string
	syncer        ControlPlaneVersionRolloutSyncer
	fleetDBClient fleetcosmosstorage.FleetDBClient
}

// NewControlPlaneVersionRolloutWatchingController watches ControlPlaneVersionRollouts
// and queues them for the provided syncer.
func NewControlPlaneVersionRolloutWatchingController(
	name string,
	fleetDBClient fleetcosmosstorage.FleetDBClient,
	fleetInformers fleetinformers.FleetInformers,
	resyncDuration time.Duration,
	syncer ControlPlaneVersionRolloutSyncer,
) Controller {
	rolloutSyncer := &controlPlaneVersionRolloutWatchingController{
		name:          name,
		syncer:        syncer,
		fleetDBClient: fleetDBClient,
	}
	rolloutController := newGenericWatchingController(name, fleetapi.ControlPlaneVersionRolloutResourceType, rolloutSyncer)

	// this happens when unit tests don't want triggering.  This isn't beautiful, but fails to do nothing which is pretty safe.
	if fleetInformers != nil {
		rolloutInformer, _ := fleetInformers.ControlPlaneVersionRollouts()
		err := rolloutController.QueueForInformers(resyncDuration, rolloutInformer)
		if err != nil {
			panic(err) // coding error
		}
	}

	return rolloutController
}

func (c *controlPlaneVersionRolloutWatchingController) SyncOnce(ctx context.Context, key ControlPlaneVersionRolloutKey) error {
	controllerCRUD := c.fleetDBClient.ControlPlaneVersionRollouts().Controllers(key.Channel)

	defer utilruntime.HandleCrash(DegradedControllerPanicHandler(
		ctx,
		controllerCRUD,
		c.name,
		key.InitialController))

	syncErr := c.syncer.SyncOnce(ctx, key)

	controllerWriteErr := WriteController(
		ctx,
		controllerCRUD,
		c.name,
		key.InitialController,
		ReportSyncError(syncErr),
	)

	return errors.Join(syncErr, controllerWriteErr)
}

func (c *controlPlaneVersionRolloutWatchingController) CooldownChecker() controllerutil.CooldownChecker {
	return c.syncer.CooldownChecker()
}

func (c *controlPlaneVersionRolloutWatchingController) MakeKey(resourceID *azcorearm.ResourceID) ControlPlaneVersionRolloutKey {
	return ControlPlaneVersionRolloutKey{
		Channel: resourceID.Name,
	}
}
