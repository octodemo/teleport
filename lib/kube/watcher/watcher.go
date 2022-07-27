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
	"reflect"
	"time"

	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/gravitational/trace"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
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
	// GetAuthConfig returns the authentication config to gain access to the cluster.
	// The second returning parameter is the expiration date of the credentials.
	// After that date they no longer provide access to the cluster. nil date means the credentials
	// do not need to be revalidated.
	GetAuthConfig() (*rest.Config, *time.Time, error)
}

// ActionFunc is a callback function that Create/Update/Delete operations for discovered Kubernetes Clusters.
// Types that are valid to be received in Cluster:
//	*EKSCluster
// TODO: add retry on return so users can decide if they want to retry or not
type ActionFunc func(context.Context, Operation, Cluster) error

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
	return e.Name
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

// GetAuthConfig returns the authentication config to gain access to the cluster.
// The second returning parameter is the expiration time of the credentials.
// nil means the credentials do not need to be revalidated.
func (e *EKSCluster) GetAuthConfig() (*rest.Config, *time.Time, error) {
	// generate temporary Bearer token  to access EKS cluster
	// this token is short-lived (the TTL is defined at the AWS IAM Role level) and should be revalidated
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	tok, err := gen.GetWithOptions(
		&token.GetTokenOptions{
			ClusterID: e.Name,
			Session:   e.AWSSession,
		},
	)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	var expDate *time.Time
	if !tok.Expiration.IsZero() {
		expDate = &tok.Expiration
	}

	return &rest.Config{
		Host:        e.APIEndpoint,
		BearerToken: tok.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: e.CAData,
		},
	}, expDate, nil
}

func equalClusters(c1, c2 Cluster) bool {
	if c1.GetName() != c2.GetName() {
		return false
	}
	if c1.GetAPIEndpoint() != c2.GetAPIEndpoint() {
		return false
	}
	if c1.GetRegion() != c2.GetRegion() {
		return false
	}
	if !reflect.DeepEqual(c1.GetLabels(), c2.GetLabels()) {
		return false
	}
	if !reflect.DeepEqual(c1.GetCAData(), c2.GetCAData()) {
		return false
	}
	return true
}
