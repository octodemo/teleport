/*
Copyright 2018-2022 Gravitational, Inc.

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

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/kube/watcher"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/transport"
)

func (f *Forwarder) removeKubeCluster(name string) error {
	f.rwMutexCreds.Lock()
	if creds, ok := f.creds[name]; ok {
		creds.close()
	}
	delete(f.creds, name)
	f.rwMutexCreds.Unlock()
	f.mu.Lock()
	// close active sessions
	var errs []error
	for _, sess := range f.sessions {
		if sess.ctx.kubeCluster == name {
			// TODO: check if we should sebd errors to client
			errs = append(errs, sess.Close())
		}
	}
	f.mu.Unlock()
	return trace.NewAggregate(errs...)
}

func (f *Forwarder) addKubeCluster(cluster watcher.Cluster) error {
	dynKubeCreds, err := newDynamicCreds(cluster, f.getStaticLabels(), f.log)
	if err != nil {
		return trace.Wrap(err)
	}

	f.rwMutexCreds.Lock()
	f.creds[cluster.GetName()] = dynKubeCreds
	f.rwMutexCreds.Unlock()

	return nil
}

func (f *Forwarder) updateKubeCluster(cluster watcher.Cluster) error {
	f.rwMutexCreds.Lock()
	creds, ok := f.creds[cluster.GetName()]
	if !ok {
		return fmt.Errorf("cluster %s not found", cluster.GetName())
	}
	f.rwMutexCreds.Unlock()

	dynCreds, ok := creds.(*dynamicCreds)
	if !ok {
		return fmt.Errorf("creds is not *dynamicCreds, instead it is %T", dynCreds)
	}
	return trace.Wrap(dynCreds.updateCluster(cluster))
}

func newDynamicCreds(cluster watcher.Cluster, staticLabels map[string]string, log logrus.FieldLogger) (*dynamicCreds, error) {
	dn := &dynamicCreds{
		cluster:             cluster,
		renewTicker:         time.NewTicker(1 * time.Hour), // this will be reseted by renewClientset
		closeC:              make(chan struct{}),
		serviceStaticLabels: staticLabels,
		log:                 log,
	}

	if err := dn.renewClientset(); err != nil {
		return nil, trace.Wrap(err)
	}

	go func() {
		select {
		case <-dn.closeC:
			dn.closeC <- struct{}{}
			return
		case <-dn.renewTicker.C:
			if err := dn.renewClientset(); err != nil {
				log.WithError(err).Warnf("unable to renew cluster \"%s\" credentials", cluster.GetName())
			}
		}
	}()
	return dn, nil
}

type dynamicCreds struct {
	cluster             watcher.Cluster
	renewTicker         *time.Ticker
	st                  *staticKubeCreds
	serviceStaticLabels map[string]string
	log                 logrus.FieldLogger
	closeC              chan struct{}
	sync.RWMutex
}

func (d *dynamicCreds) getTLSConfig() *tls.Config {
	d.RLock()
	defer d.RUnlock()
	return d.st.tlsConfig
}
func (d *dynamicCreds) getTransportConfig() *transport.Config {
	d.RLock()
	defer d.RUnlock()
	return d.st.transportConfig
}
func (d *dynamicCreds) getTargetAddr() string {
	d.RLock()
	defer d.RUnlock()
	return d.st.targetAddr
}
func (d *dynamicCreds) getKubeClient() *kubernetes.Clientset {
	d.RLock()
	defer d.RUnlock()
	return d.st.kubeClient
}
func (d *dynamicCreds) wrapTransport(rt http.RoundTripper) (http.RoundTripper, error) {
	d.RLock()
	defer d.RUnlock()
	return d.st.wrapTransport(rt)
}

func (d *dynamicCreds) getStaticLabels() map[string]string {
	d.RLock()
	defer d.RUnlock()
	return d.st.staticLabels
}

// updateCluster updates the cluster and renews the access token.
func (d *dynamicCreds) updateCluster(cluster watcher.Cluster) error {
	d.Lock()
	d.cluster = cluster
	d.Unlock()
	return trace.Wrap(d.renewClientset())
}

// close closes the credentials renewal goroutine.
func (c *dynamicCreds) close() error {
	c.Lock()
	defer c.Unlock()
	c.closeC <- struct{}{}
	<-c.closeC
	return nil
}

// renewClientset generates the credentials required for accessing the cluster using the GetAuthConfig function provided by watcher.
func (d *dynamicCreds) renewClientset() error {
	// get auth config
	restConfig, exp, err := d.cluster.GetAuthConfig()
	if err != nil {
		return trace.Wrap(err)
	}

	creds, err := newStaticKubeCreds(
		context.TODO(),
		d.cluster.GetName(),
		restConfig,
		checkImpersonationPermissions,
	)
	if err != nil {
		return trace.Wrap(err)
	}

	d.Lock()
	defer d.Unlock()
	d.st = creds
	// update the static labels if updated
	d.genStaticLabelsFromCluster()
	// prepares the next renew cycle
	if exp != nil {
		d.renewTicker.Reset(time.Until(*exp) / 2)
	}
	return nil
}

// genStaticLabelsFromCluster generates labels for the discovered cluster.
// This function imports cloud cluster tags as labels and appends the static service labels on top of them.
// If cluster tags and static service labels have colision keys, service static label will replace the cluster tag.
func (d *dynamicCreds) genStaticLabelsFromCluster() {
	labels := map[string]string{}
	maps.Copy(labels, d.cluster.GetLabels())
	for k, v := range d.serviceStaticLabels {
		labels[k] = v
	}
	// force "teleport.dev/origin:cloud" label
	labels[types.OriginLabel] = types.OriginCloud
	d.st.staticLabels = labels
}
