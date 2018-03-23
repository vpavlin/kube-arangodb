//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package deployment

import (
	"context"
	"sync"
	"time"

	api "github.com/arangodb/kube-arangodb/pkg/apis/deployment/v1alpha"
	"github.com/arangodb/kube-arangodb/pkg/util/arangod"
	"github.com/rs/zerolog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clusterScalingIntegration is a helper to communicate with the clusters
// scaling UI.
type clusterScalingIntegration struct {
	log           zerolog.Logger
	depl          *Deployment
	pendingUpdate struct {
		mutex sync.Mutex
		spec  *api.DeploymentSpec
	}
	lastNumberOfServers struct {
		arangod.NumberOfServers
		mutex sync.Mutex
	}
}

// newClusterScalingIntegration creates a new clusterScalingIntegration.
func newClusterScalingIntegration(depl *Deployment) *clusterScalingIntegration {
	return &clusterScalingIntegration{
		log:  depl.deps.Log,
		depl: depl,
	}
}

// SendUpdateToCluster records the given spec to be sended to the cluster.
func (ci *clusterScalingIntegration) SendUpdateToCluster(spec api.DeploymentSpec) {
	ci.pendingUpdate.mutex.Lock()
	defer ci.pendingUpdate.mutex.Unlock()
	ci.pendingUpdate.spec = &spec
}

// listenForClusterEvents keep listening for changes entered in the UI of the cluster.
func (ci *clusterScalingIntegration) ListenForClusterEvents(stopCh <-chan struct{}) {
	for {
		delay := time.Second * 2

		// Is deployment in running state
		if ci.depl.status.State == api.DeploymentStateRunning {
			// Update cluster with our state
			ctx := context.Background()
			safeToAskCluster, err := ci.updateClusterServerCount(ctx)
			if err != nil {
				ci.log.Debug().Err(err).Msg("Cluster update failed")
			} else if safeToAskCluster {
				// Inspect once
				if err := ci.inspectCluster(ctx); err != nil {
					ci.log.Debug().Err(err).Msg("Cluster inspection failed")
				}
			}
		}

		select {
		case <-time.After(delay):
			// Continue
		case <-stopCh:
			// We're done
			return
		}
	}
}

// Perform a single inspection of the cluster
func (ci *clusterScalingIntegration) inspectCluster(ctx context.Context) error {
	log := ci.log
	c, err := ci.depl.clientCache.GetDatabase(ctx)
	if err != nil {
		return maskAny(err)
	}
	req, err := arangod.GetNumberOfServers(ctx, c.Connection())
	if err != nil {
		log.Debug().Err(err).Msg("Failed to get number of servers")
		return maskAny(err)
	}
	if req.Coordinators == nil && req.DBServers == nil {
		// Nothing to check
		return nil
	}
	coordinatorsChanged := false
	dbserversChanged := false
	ci.lastNumberOfServers.mutex.Lock()
	defer ci.lastNumberOfServers.mutex.Unlock()
	desired := ci.lastNumberOfServers.NumberOfServers
	if req.Coordinators != nil && desired.Coordinators != nil && req.GetCoordinators() != desired.GetCoordinators() {
		// #Coordinator has changed
		coordinatorsChanged = true
	}
	if req.DBServers != nil && desired.DBServers != nil && req.GetDBServers() != desired.GetDBServers() {
		// #DBServers has changed
		dbserversChanged = true
	}
	if !coordinatorsChanged && !dbserversChanged {
		// Nothing has changed
		return nil
	}
	// Let's update the spec
	apiObject := ci.depl.apiObject
	current, err := ci.depl.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(apiObject.Namespace).Get(apiObject.Name, metav1.GetOptions{})
	if err != nil {
		log.Debug().Err(err).Msg("Failed to get current deployment")
		return maskAny(err)
	}
	if coordinatorsChanged {
		current.Spec.Coordinators.Count = req.GetCoordinators()
	}
	if dbserversChanged {
		current.Spec.DBServers.Count = req.GetDBServers()
	}
	if err := ci.depl.updateCRSpec(current.Spec); err != nil {
		log.Warn().Err(err).Msg("Failed to update current deployment")
		return maskAny(err)
	}
	return nil
}

// updateClusterServerCount updates the intended number of servers of the cluster.
// Returns true when it is safe to ask the cluster for updates.
func (ci *clusterScalingIntegration) updateClusterServerCount(ctx context.Context) (bool, error) {
	// Any update needed?
	ci.pendingUpdate.mutex.Lock()
	spec := ci.pendingUpdate.spec
	ci.pendingUpdate.mutex.Unlock()
	if spec == nil {
		// Nothing pending
		return true, nil
	}

	log := ci.log
	c, err := ci.depl.clientCache.GetDatabase(ctx)
	if err != nil {
		return false, maskAny(err)
	}
	coordinatorCount := spec.Coordinators.Count
	dbserverCount := spec.DBServers.Count
	if err := arangod.SetNumberOfServers(ctx, c.Connection(), coordinatorCount, dbserverCount); err != nil {
		log.Debug().Err(err).Msg("Failed to set number of servers")
		return false, maskAny(err)
	}

	// Success, now update internal state
	safeToAskCluster := false
	ci.pendingUpdate.mutex.Lock()
	if spec == ci.pendingUpdate.spec {
		ci.pendingUpdate.spec = nil
		safeToAskCluster = true
	}
	ci.pendingUpdate.mutex.Unlock()

	ci.lastNumberOfServers.mutex.Lock()
	defer ci.lastNumberOfServers.mutex.Unlock()

	ci.lastNumberOfServers.Coordinators = &coordinatorCount
	ci.lastNumberOfServers.DBServers = &dbserverCount
	return safeToAskCluster, nil
}
