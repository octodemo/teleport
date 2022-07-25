/*
Copyright 2022 Gravitational, Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watcher

import (
	"context"

	awssession "github.com/aws/aws-sdk-go/aws/session"
)

// Operation defines the operation
type Operation int

const (
	// OperationCreate is used when a new cluster is discovered
	OperationCreate Operation = iota + 1
	// OperationUpdate is used when a cluster is updated (eg API url, CA or static labels).
	OperationUpdate
	// OperationDelete is used when a cluster is deleted or does not match the target labels.
	OperationDelete
)

// Cluster is an interface that describes multiple Kubernetes clusters
type Cluster interface {
	// GetName returns the cluster name.
	GetName() string
	// GetRegion returns the region where the cluster is located.
	GetRegion() string
	// GetAPIEndpoint returns the Kubernetes cluster API endpoint.
	GetAPIEndpoint() string
	// GetCAData returns the certificate authority certificate data for API server.
	GetCAData() []byte
	// GetLabels returns the sanitized labels ready to be used in Teleport.
	GetLabels() map[string]string
}

// ActionFunc is a callback function that Create/Update/Delete operations for discovered Kubernetes Clusters.
// Types that are valid to be received in Cluster:
//	*EKSCluster
type ActionFunc func(context.Context, Operation, Cluster)

//EKSCluster represents a discovered EKS in AWS.
type EKSCluster struct {
	//Name is the cluster name.
	Name string
	// Region is the region where the cluster is located.
	Region string
	// APIEndpoint is the Kubernetes cluster API endpoint.
	APIEndpoint string
	// CAData is the certificate authority certificate data for API server.
	CAData []byte
	// Labels is a kv map of sanitized labels ready to be used in Teleport.
	Labels map[string]string
	// AWSSession is the session that can be used to generate dynamic access tokens.
	AWSSession *awssession.Session
}

// GetName returns the cluster name.
func (e *EKSCluster) GetName() string {
	return e.APIEndpoint
}

// GetRegion returns the region where the cluster is located.
func (e *EKSCluster) GetRegion() string {
	return e.Region
}

// GetCAData returns the certificate authority certificate data for API server.
func (e *EKSCluster) GetCAData() []byte {
	return e.CAData
}

// GetAPIEndpoint returns the Kubernetes cluster API endpoint.
func (e *EKSCluster) GetAPIEndpoint() string {
	return e.APIEndpoint
}

// GetLabels returns the sanitized labels ready to be used in Teleport.
func (e *EKSCluster) GetLabels() map[string]string {
	return e.Labels
}
