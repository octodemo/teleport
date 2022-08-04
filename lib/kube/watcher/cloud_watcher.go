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
	"time"

	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/db/common"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// Cpmf
type Config struct {
	AWSMatchers  []services.AWSMatcher
	CloudClients common.CloudClients
	Action       ActionFunc
	Log          logrus.FieldLogger
}
type Watcher struct {
	fetchers []fetcher
	waitTime time.Duration
	log      logrus.FieldLogger
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewWatcher(
	ctx context.Context,
	cfg Config,
) (*Watcher, error) {
	cancelCtx, cancelFn := context.WithCancel(ctx)
	watcher := Watcher{
		fetchers: []fetcher{},
		ctx:      cancelCtx,
		cancel:   cancelFn,
		waitTime: time.Minute,
		log:      cfg.Log,
	}
	for _, matcher := range cfg.AWSMatchers {
		for _, region := range matcher.Regions {
			awsSession, err := cfg.CloudClients.GetAWSSession(region)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			cl, err := cfg.CloudClients.GetAWSEKSClient(region)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			fetcher, err := newEKSClusterFetcher(matcher, region, awsSession, cl, cfg.Action, cfg.Log)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			watcher.fetchers = append(watcher.fetchers, fetcher)
		}
	}
	return &watcher, nil
}

func (w *Watcher) Start() {
	w.log.Debugf("Kubernetes Cluster discovery started with polling interval of %s", w.waitTime)

	ticker := time.NewTicker(w.waitTime)
	defer ticker.Stop()
	for {
		for _, fetcher := range w.fetchers {
			err := fetcher.FetchKubeClusters(w.ctx)
			if err != nil {
				w.log.WithError(err).Error("Failed to fetch EKS clusters")
			}
		}
		select {
		case <-ticker.C:
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *Watcher) Close() {
	w.cancel()
}

type fetcher interface {
	FetchKubeClusters(context.Context) error
}
