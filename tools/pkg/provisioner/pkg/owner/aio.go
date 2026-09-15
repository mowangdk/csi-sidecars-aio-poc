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
	"fmt"

	"github.com/kubernetes-csi/csi-sidecars/pkg/csistartup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LookupAIO preserves Lookup's ownership semantics while binding discovery and
// every GET to the runner, and retaining cancellation identity through errors.
func LookupAIO(ctx context.Context, config *rest.Config, namespace, name string, gkv schema.GroupVersionKind, levels int) (*metav1.OwnerReference, error) {
	c, err := client.New(csistartup.OwnConfig(ctx, config), client.Options{})
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return lookupAIO(ctx, c, namespace, name, gkv, levels)
}

func lookupAIO(ctx context.Context, c client.Client, namespace, name string, gkv schema.GroupVersionKind, levels int) (*metav1.OwnerReference, error) {
	for {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(gkv)
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
			return nil, fmt.Errorf("get object: %w", err)
		}
		controller := true
		if levels == 0 {
			return &metav1.OwnerReference{APIVersion: object.GetAPIVersion(), Kind: gkv.Kind, Name: name, UID: object.GetUID(), Controller: &controller}, nil
		}
		var next *metav1.OwnerReference
		for _, owner := range object.GetOwnerReferences() {
			if owner.Controller != nil && *owner.Controller {
				next = &owner
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("%s/%s %q in namespace %q has no controlling owner, cannot unwind the ownership further", object.GetAPIVersion(), gkv.Kind, name, namespace)
		}
		gv, err := schema.ParseGroupVersion(next.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("parse OwnerReference.APIVersion: %w", err)
		}
		if levels == 1 {
			return &metav1.OwnerReference{APIVersion: next.APIVersion, Kind: next.Kind, Name: next.Name, UID: next.UID, Controller: &controller}, nil
		}
		gkv, name, levels = gv.WithKind(next.Kind), next.Name, levels-1
	}
}
