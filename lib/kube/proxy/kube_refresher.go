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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
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
	var (
		dynKubeCreds kubeCreds
		err          error
	)

	switch t := cluster.(type) {
	case *watcher.EKSCluster:
		dynKubeCreds, err = newEKSDynamicCreds(t, f.getStaticLabels(), f.log)
	default:
		return fmt.Errorf("unknown type for cluster: %T", cluster)
	}
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

	switch t := cluster.(type) {
	case *watcher.EKSCluster:
		dynCreds, ok := creds.(*eksDynamicCreds)
		if !ok {
			return fmt.Errorf("creds is not *eksDynamicCreds, instead it is %T", dynCreds)
		}
		return trace.Wrap(dynCreds.updateCluster(t))
	default:
		return fmt.Errorf("unknown type for cluster: %T", cluster)
	}

}

func newEKSDynamicCreds(cluster *watcher.EKSCluster, staticLabels map[string]string, log logrus.FieldLogger) (*eksDynamicCreds, error) {
	dn := &eksDynamicCreds{
		eksCluster:          cluster,
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
				// TODO: add loggs
				_ = err
			}
		}
	}()
	return dn, nil
}

type eksDynamicCreds struct {
	eksCluster          *watcher.EKSCluster
	renewTicker         *time.Ticker
	st                  *staticKubeCreds
	serviceStaticLabels map[string]string
	log                 logrus.FieldLogger
	sync.RWMutex
	closeC chan struct{}
}

func (c *eksDynamicCreds) close() error {
	c.Lock()
	defer c.Unlock()
	c.closeC <- struct{}{}
	<-c.closeC
	return nil
}

func (d *eksDynamicCreds) tlsConfiguration() *tls.Config {
	d.RLock()
	defer d.RUnlock()
	return d.st.tlsConfig
}
func (d *eksDynamicCreds) transportConfiguration() *transport.Config {
	d.RLock()
	defer d.RUnlock()
	return d.st.transportConfig
}
func (d *eksDynamicCreds) targetAddress() string {
	d.RLock()
	defer d.RUnlock()
	return d.st.targetAddr
}
func (d *eksDynamicCreds) kubernetesClient() *kubernetes.Clientset {
	d.RLock()
	defer d.RUnlock()
	return d.st.kubeClient
}
func (d *eksDynamicCreds) wrapTransport(rt http.RoundTripper) (http.RoundTripper, error) {
	d.RLock()
	defer d.RUnlock()
	return d.st.wrapTransport(rt)
}

func (d *eksDynamicCreds) getStaticLabels() map[string]string {
	d.RLock()
	defer d.RUnlock()
	return d.st.staticLabels
}

func (d *eksDynamicCreds) updateCluster(cluster *watcher.EKSCluster) error {
	d.RLock()
	d.eksCluster = cluster
	d.RUnlock()
	return trace.Wrap(d.renewClientset())
}

func (d *eksDynamicCreds) renewClientset() error {
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return trace.Wrap(err)
	}

	opts := &token.GetTokenOptions{
		ClusterID: d.eksCluster.Name,
		Session:   d.eksCluster.AWSSession,
	}
	tok, err := gen.GetWithOptions(opts)
	if err != nil {
		return trace.Wrap(err)
	}

	creds, err := extractKubeCreds(
		context.TODO(),
		d.eksCluster.Name,
		&rest.Config{
			Host:        d.eksCluster.APIEndpoint,
			BearerToken: tok.Token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: d.eksCluster.CAData,
			},
		},
		KubeService,
		"",
		d.log,
		checkImpersonationPermissions,
		nil,
	)
	if err != nil {
		return trace.Wrap(err)
	}

	d.Lock()
	defer d.Unlock()
	d.st = creds
	d.genStaticLabelsFromEKS()
	d.renewTicker.Reset(time.Until(tok.Expiration) / 2)
	return nil
}

func (d *eksDynamicCreds) genStaticLabelsFromEKS() {
	labels := map[string]string{}
	maps.Copy(labels, d.eksCluster.Labels)
	for k, v := range d.serviceStaticLabels {
		labels[k] = v
	}
	labels[types.OriginLabel] = types.OriginCloud
	d.st.staticLabels = labels
}
