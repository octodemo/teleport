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
	"encoding/base64"
	"reflect"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/eks/eksiface"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

type eksClusterFetcher struct {
	filterLabels types.Labels
	eksClient    eksiface.EKSAPI
	region       string
	mu           sync.Mutex
	action       ActionFunc
	cache        map[string]cacheEntry
	log          logrus.FieldLogger
	session      *awssession.Session
}

func newEKSClusterFetcher(matcher services.AWSMatcher, region string, session *awssession.Session, eksClient eksiface.EKSAPI, action ActionFunc, log logrus.FieldLogger) (*eksClusterFetcher, error) {
	return &eksClusterFetcher{
		eksClient:    eksClient,
		filterLabels: matcher.Tags,
		region:       region,
		cache:        map[string]cacheEntry{},
		action:       action,
		session:      session,
		log: log.WithFields(logrus.Fields{
			"discovery": "kube",
			"type":      "eks",
			"region":    region,
		}),
	}, nil
}

type cacheEntry struct {
	lastSeen time.Time
	cluster  *EKSCluster
}

func (f *eksClusterFetcher) FetchKubeClusters(ctx context.Context) error {
	t1 := time.Now()

	err := f.eksClient.ListClustersPagesWithContext(ctx,
		&eks.ListClustersInput{
			Include: nil, // For now we should only list EKS clusters
		},
		func(clustersList *eks.ListClustersOutput, _ bool) bool {
			wg := &sync.WaitGroup{}
			wg.Add(len(clustersList.Clusters))
			for i := 0; i < len(clustersList.Clusters); i++ {
				go func(clusterName string) {
					defer wg.Done()
					logger := f.log.WithField("cluster_name", clusterName)

					rsp, err := f.eksClient.DescribeClusterWithContext(
						ctx,
						&eks.DescribeClusterInput{
							Name: aws.String(clusterName),
						},
					)
					if err != nil {
						logger.WithError(err).Warnf("Unable to describe EKS cluster: %v", err)
						return
					}

					eksCluster, err := f.newEKSClusterFromCluster(rsp.Cluster)
					if err != nil {
						logger.Warnf("unable to convert eks.Cluster into EKSCLuster: %v", err)
						return
					}

					if match, reason, err := services.MatchLabels(f.filterLabels, eksCluster.Labels); err != nil {
						logger.WithError(err).Warnf("Unable to match EKS cluster labels against match labels: %v", err)
						return
					} else if !match {
						f.mu.Lock()
						defer f.mu.Unlock()
						if _, ok := f.cache[clusterName]; !ok {
							logger.Debugf("EKS cluster labels does not match the selector: %s", reason)
						} else {
							delete(f.cache, clusterName)
							f.action(ctx, OperationDelete, eksCluster)
						}
						return
					}

					// delete is triggered by other operation
					var operation Operation
					f.mu.Lock()
					switch status := aws.StringValue(rsp.Cluster.Status); status {
					case eks.ClusterStatusCreating, eks.ClusterStatusPending, eks.ClusterStatusFailed:
						logger.Debugf("EKS cluster not ready: status=%s", status)
						return
					case eks.ClusterStatusActive, eks.ClusterStatusUpdating:
						if entry, ok := f.cache[clusterName]; ok {
							// todo: validate the equality for eks clusters: endpoints + labels + CA
							if reflect.DeepEqual(entry.cluster, eksCluster) {
								// eks cluster has the same config as before.
								// doing nothing and returning early since we do not need to update it.
								return
							}
							operation = OperationUpdate
						} else {
							operation = OperationCreate
						}

						f.cache[clusterName] = cacheEntry{
							lastSeen: time.Now(),
							cluster:  eksCluster,
						}
					case eks.ClusterStatusDeleting:
						operation = OperationDelete
						// clear object from cache
						delete(f.cache, clusterName)
					}
					f.mu.Unlock()
					f.action(ctx, operation, eksCluster)
				}(aws.StringValue(clustersList.Clusters[i]))
			}
			wg.Wait()
			return true
		},
	)

	f.mu.Lock()
	defer f.mu.Unlock()
	deletions := make([]string, 0, len(f.cache))
	for k, v := range f.cache {
		//  if last time we saw the cluster was in the previous iteration, than we should delete it since it's no longer available
		// TODO: check if we should check twice the time.
		if v.lastSeen.Before(t1) {
			deletions = append(deletions, k)
			f.action(ctx, OperationDelete, v.cluster)
		}
	}

	for _, d := range deletions {
		delete(f.cache, d)
	}

	return trace.Wrap(err)

}

func (f *eksClusterFetcher) eksTagsToLabels(tags map[string]*string) map[string]string {
	labels := make(map[string]string)
	for key, valuePtr := range tags {
		if types.IsValidLabelKey(key) {
			labels[key] = aws.StringValue(valuePtr)
		} else {
			f.log.Debugf("Skipping EKS tag %q, not a valid label key", key)
		}
	}
	return labels
}

func (f *eksClusterFetcher) newEKSClusterFromCluster(cluster *eks.Cluster) (*EKSCluster, error) {
	ca, err := base64.StdEncoding.DecodeString(aws.StringValue(cluster.CertificateAuthority.Data))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusterLabels := f.eksTagsToLabels(cluster.Tags)

	return &EKSCluster{
		Name:        aws.StringValue(cluster.Name),
		Region:      f.region,
		APIEndpoint: aws.StringValue(cluster.Endpoint),
		CAData:      ca,
		Labels:      clusterLabels,
		AWSSession:  f.session,
	}, nil
}
