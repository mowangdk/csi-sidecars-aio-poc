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

package owner

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ownerClient struct {
	client.Client
	get func(context.Context, client.ObjectKey, client.Object) error
}

func (c ownerClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return c.get(ctx, key, obj)
}

func TestLookupAIOLevels(t *testing.T) {
	for level := 0; level <= 2; level++ {
		calls := 0
		c := ownerClient{get: func(ctx context.Context, key client.ObjectKey, obj client.Object) error {
			calls++
			if key.Namespace != "test" {
				t.Fatal("namespace changed")
			}
			obj.SetUID(types.UID(key.Name + "-uid"))
			controller := true
			owner := "deployment"
			if key.Name == "pod" {
				owner = "replicaset"
			}
			obj.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Owner", Name: owner, UID: types.UID(owner + "-uid"), Controller: &controller}})
			return nil
		}}
		got, err := lookupAIO(context.Background(), c, "test", "pod", schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, level)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"pod", "replicaset", "deployment"}[level]
		if got.Name != want || string(got.UID) != want+"-uid" || got.Controller == nil || !*got.Controller {
			t.Fatalf("level %d: %#v", level, got)
		}
		wantCalls := level
		if level == 0 {
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Fatalf("level %d: %d GETs", level, calls)
		}
	}
}

func TestLookupAIOCancellationAndErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	c := ownerClient{get: func(request context.Context, _ client.ObjectKey, _ client.Object) error {
		close(started)
		<-request.Done()
		return request.Err()
	}}
	done := make(chan error, 1)
	go func() {
		_, err := lookupAIO(ctx, c, "test", "pod", schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, 0)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	c.get = func(context.Context, client.ObjectKey, client.Object) error { return nil }
	if _, err := lookupAIO(context.Background(), c, "test", "pod", (&unstructured.Unstructured{}).GroupVersionKind(), 1); err == nil {
		t.Fatal("accepted missing controller owner")
	}
}
