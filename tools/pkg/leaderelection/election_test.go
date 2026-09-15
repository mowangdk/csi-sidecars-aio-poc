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
	"errors"
	"testing"
	"testing/synctest"
	"time"

	aioruntime "github.com/kubernetes-csi/csi-sidecars/pkg/runtime"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	clientleaderelection "k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func electionConfig(t *testing.T, client *fake.Clientset, release bool) clientleaderelection.LeaderElectionConfig {
	t.Helper()
	lock, err := resourcelock.NewWithLabels(resourcelock.LeasesResourceLock, "test", "controller", client.CoreV1(), client.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: "this-pod"}, map[string]string{"controller": "attacher"})
	if err != nil {
		t.Fatal(err)
	}
	return clientleaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 4 * time.Second, RenewDeadline: 2 * time.Second,
		RetryPeriod: 200 * time.Millisecond, ReleaseOnCancel: release,
	}
}

func lease(t *testing.T, client *fake.Clientset) *coordinationv1.Lease {
	t.Helper()
	value, err := client.CoordinationV1().Leases("test").Get(context.Background(), "controller", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestLeaderRenewsWhileDraining(t *testing.T) {
	for _, releaseOnExit := range []bool{false, true} {
		t.Run(map[bool]string{false: "expire", true: "release"}[releaseOnExit], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := fake.NewClientset()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started, draining, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- Run(ctx, electionConfig(t, client, releaseOnExit), func(ctx context.Context) error {
						close(started)
						<-ctx.Done()
						close(draining)
						<-drained
						return nil
					})
				}()
				<-started
				cancel()
				<-draining
				previous := lease(t, client).Spec.RenewTime.Time
				// Virtual time proves renewal continues beyond an entire lease.
				time.Sleep(5 * time.Second)
				current := lease(t, client)
				if *current.Spec.HolderIdentity != "this-pod" || !current.Spec.RenewTime.Time.After(previous) {
					t.Fatalf("lease not retained and renewed while draining: %+v", current.Spec)
				}
				close(drained)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				want := "this-pod"
				if releaseOnExit {
					want = ""
				}
				if got := *lease(t, client).Spec.HolderIdentity; got != want {
					t.Fatalf("holder=%q, want %q", got, want)
				}
			})
		})
	}
}

func TestFollowerShutdownDoesNotWaitForWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		holder, seconds := "other-pod", int32(60)
		now := metav1.NewMicroTime(time.Now())
		client := fake.NewClientset(&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "controller"},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &now},
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, electionConfig(t, client, true), func(context.Context) error {
				t.Error("follower started workers")
				return nil
			})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if *lease(t, client).Spec.HolderIdentity != holder {
			t.Fatal("follower modified holder")
		}
	})
}

func TestLeadershipLossCancelsPeersBeforeDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := fake.NewClientset()
		started, draining, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
		peerStopped := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- aioruntime.Supervise(context.Background(), 20*time.Second, map[string]aioruntime.Runner{
				"attacher": func(ctx context.Context) error {
					return Run(ctx, electionConfig(t, client, true), func(ctx context.Context) error {
						close(started)
						<-ctx.Done()
						close(draining)
						<-drained
						return nil
					})
				},
				"resizer": func(ctx context.Context) error { <-ctx.Done(); close(peerStopped); return nil },
			})
		}()
		<-started
		current := lease(t, client)
		replacement := "new-leader"
		current.Spec.HolderIdentity = &replacement
		// The fake tracker does not enforce resourceVersion. Model the API's
		// conflict response so client-go's cached optimistic renewal cannot
		// overwrite the replacement holder without re-reading the lease.
		client.PrependReactor("update", "leases", func(action clienttesting.Action) (bool, runtime.Object, error) {
			update := action.(clienttesting.UpdateAction).GetObject().(*coordinationv1.Lease)
			if update.Spec.HolderIdentity != nil && *update.Spec.HolderIdentity == "this-pod" {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "controller", errors.New("stale resourceVersion"))
			}
			return false, nil, nil
		})
		_, err := client.CoordinationV1().Leases("test").Update(context.Background(), current, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		<-draining
		<-peerStopped
		if *lease(t, client).Spec.HolderIdentity != replacement {
			t.Fatal("lease released before drain")
		}
		close(drained)
		if err := <-done; !errors.Is(err, ErrLeadershipLost) {
			t.Fatalf("loss error=%v", err)
		}
		if *lease(t, client).Spec.HolderIdentity != replacement {
			t.Fatal("released another leader's lease")
		}
	})
}

func TestCancellationPreservesWorkerFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := fake.NewClientset()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		done := make(chan error, 1)
		failure := errors.New("worker failed while draining")
		go func() {
			done <- Run(ctx, electionConfig(t, client, true), func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return failure
			})
		}()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, failure) {
			t.Fatalf("lost worker failure: %v", err)
		}
	})
}
