/*
Copyright 2026 The Kubernetes Authors.

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

package leaderelection

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kubernetes-csi/csi-lib-utils/standardflags"
	aioruntime "github.com/kubernetes-csi/csi-sidecars/pkg/runtime"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clientleaderelection "k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// RunWithLeaderElection preserves csi-lib-utils' per-controller lease naming,
// hostname identity, namespace selection, labels, timers, and health-check path.
// Only lifecycle/exit ownership differs from the standalone wrapper.
func RunWithLeaderElection(ctx context.Context, config *rest.Config, opts standardflags.SidecarConfiguration, run aioruntime.Runner, lockName string, mux *http.ServeMux, releaseOnExit bool) error {
	if !opts.LeaderElection {
		return run(ctx)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create leader-election client: %w", err)
	}
	identity, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get leader-election identity: %w", err)
	}
	if identity == "" || lockName == "" {
		return fmt.Errorf("empty leader-election identity or lock name")
	}
	namespace := opts.LeaderElectionNamespace
	if namespace == "" {
		namespace = inClusterNamespace()
	}
	logger := klog.FromContext(ctx)
	eventCtx, cancelEvents := context.WithCancel(context.WithoutCancel(ctx))
	broadcaster := record.NewBroadcaster(record.WithContext(eventCtx))
	defer broadcaster.Shutdown()
	defer cancelEvents()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: client.CoreV1().Events(namespace)})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: lockName + "/" + identity}).WithLogger(logger)
	lock, err := resourcelock.NewWithLabels(resourcelock.LeasesResourceLock, namespace, sanitizeName(lockName), client.CoreV1(), client.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: sanitizeName(identity), EventRecorder: recorder}, opts.LeaderElectionLabels)
	if err != nil {
		return fmt.Errorf("create leader-election lock: %w", err)
	}
	var health *clientleaderelection.HealthzAdaptor
	if opts.HttpEndpoint != "" {
		if mux == nil {
			return fmt.Errorf("leader-election health check requires HTTP mux")
		}
		health = clientleaderelection.NewLeaderHealthzAdaptor(20 * time.Second)
		mux.HandleFunc("/healthz/leader-election", func(w http.ResponseWriter, r *http.Request) {
			if err := health.Check(r); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, "ok")
		})
	}
	return Run(ctx, clientleaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   opts.LeaderElectionLeaseDuration,
		RenewDeadline:   opts.LeaderElectionRenewDeadline,
		RetryPeriod:     opts.LeaderElectionRetryPeriod,
		ReleaseOnCancel: releaseOnExit,
		WatchDog:        health,
		Callbacks: clientleaderelection.LeaderCallbacks{OnNewLeader: func(identity string) {
			logger.V(3).Info("New leader detected", "leader", identity)
		}},
	}, run)
}

var unsafeName = regexp.MustCompile("[^a-zA-Z0-9-]")

func sanitizeName(name string) string {
	name = unsafeName.ReplaceAllString(name, "-")
	if strings.HasSuffix(name, "-") {
		name += "X"
	}
	return name
}

func inClusterNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return "default"
}
