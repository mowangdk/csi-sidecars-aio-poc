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

package csi

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	spec "github.com/container-storage-interface/spec/lib/go/csi"
)

type observingClient struct {
	Client
	request func(context.Context) error
}

func (c observingClient) GetDriverName(ctx context.Context) (string, error) {
	return "", c.request(ctx)
}
func (c observingClient) SupportsPluginControllerService(ctx context.Context) (bool, error) {
	return false, c.request(ctx)
}
func (c observingClient) SupportsControllerResize(ctx context.Context) (bool, error) {
	return false, c.request(ctx)
}
func (c observingClient) SupportsNodeResize(ctx context.Context) (bool, error) {
	return false, c.request(ctx)
}
func (c observingClient) SupportsControllerModify(ctx context.Context) (bool, error) {
	return false, c.request(ctx)
}
func (c observingClient) SupportsControllerSingleNodeMultiWriter(ctx context.Context) (bool, error) {
	return false, c.request(ctx)
}
func (c observingClient) Expand(ctx context.Context, _ string, _ int64, _ map[string]string, _ *spec.VolumeCapability) (int64, bool, error) {
	return 0, false, c.request(ctx)
}
func (c observingClient) Modify(ctx context.Context, _ string, _, _ map[string]string) error {
	return c.request(ctx)
}

func TestAIOOwnsAllRPCContexts(t *testing.T) {
	calls := map[string]func(Client, context.Context) error{
		"name": func(c Client, ctx context.Context) error { _, err := c.GetDriverName(ctx); return err },
		"service": func(c Client, ctx context.Context) error {
			_, err := c.SupportsPluginControllerService(ctx)
			return err
		},
		"resize capability": func(c Client, ctx context.Context) error { _, err := c.SupportsControllerResize(ctx); return err },
		"node capability":   func(c Client, ctx context.Context) error { _, err := c.SupportsNodeResize(ctx); return err },
		"modify capability": func(c Client, ctx context.Context) error { _, err := c.SupportsControllerModify(ctx); return err },
		"multiwriter capability": func(c Client, ctx context.Context) error {
			_, err := c.SupportsControllerSingleNodeMultiWriter(ctx)
			return err
		},
		"expand": func(c Client, ctx context.Context) error {
			_, _, err := c.Expand(ctx, "volume", 10, nil, nil)
			return err
		},
		"modify": func(c Client, ctx context.Context) error { return c.Modify(ctx, "volume", nil, nil) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, stop := context.WithCancel(context.Background())
				defer stop()
				request, cancel := context.WithTimeout(context.Background(), 7*time.Second)
				defer cancel()
				started := make(chan struct{})
				done := make(chan error, 1)
				inner := observingClient{request: func(ctx context.Context) error {
					if got, ok := ctx.Deadline(); !ok || time.Until(got) != 7*time.Second {
						t.Error("RPC deadline changed")
					}
					close(started)
					<-ctx.Done()
					return ctx.Err()
				}}
				client := &aioClient{Client: inner, parent: parent}
				go func() { done <- call(client, request) }()
				<-started
				stop()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if request.Err() != nil {
					t.Fatal("canceled caller-owned context")
				}
			})
		})
	}
}
