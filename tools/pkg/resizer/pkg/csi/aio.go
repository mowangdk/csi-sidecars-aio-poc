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
	"fmt"
	"time"

	spec "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/connection"
	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/kubernetes-csi/csi-sidecars/pkg/csistartup"
)

// NewAIO preserves New for standalone users, but gives AIO connection failures
// to its supervisor. It also bounds legacy detached RPC contexts by the runner.
func NewAIO(ctx context.Context, address string, timeout time.Duration, manager metrics.CSIMetricsManager, lost func(context.Context) bool) (Client, error) {
	conn, err := connection.Connect(ctx, address, manager, connection.OnConnectionLoss(lost))
	if err != nil {
		return nil, fmt.Errorf("connect to CSI driver: %w", err)
	}
	if err := csistartup.ProbeForever(ctx, conn, timeout); err != nil {
		conn.Close()
		return nil, fmt.Errorf("probe CSI driver: %w", err)
	}
	inner := &client{conn: conn, nodeClient: spec.NewNodeClient(conn), ctrlClient: spec.NewControllerClient(conn)}
	return &aioClient{Client: inner, parent: ctx}, nil
}

type aioClient struct {
	Client
	parent context.Context
}

func (c *aioClient) request(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.parent, cancel)
	if c.parent.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (c *aioClient) GetDriverName(ctx context.Context) (string, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.GetDriverName(ctx)
}
func (c *aioClient) SupportsPluginControllerService(ctx context.Context) (bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.SupportsPluginControllerService(ctx)
}
func (c *aioClient) SupportsControllerResize(ctx context.Context) (bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.SupportsControllerResize(ctx)
}
func (c *aioClient) SupportsNodeResize(ctx context.Context) (bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.SupportsNodeResize(ctx)
}
func (c *aioClient) SupportsControllerModify(ctx context.Context) (bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.SupportsControllerModify(ctx)
}
func (c *aioClient) SupportsControllerSingleNodeMultiWriter(ctx context.Context) (bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.SupportsControllerSingleNodeMultiWriter(ctx)
}
func (c *aioClient) Expand(ctx context.Context, id string, size int64, secrets map[string]string, capability *spec.VolumeCapability) (int64, bool, error) {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.Expand(ctx, id, size, secrets, capability)
}
func (c *aioClient) Modify(ctx context.Context, id string, parameters map[string]string, secrets map[string]string) error {
	ctx, cancel := c.request(ctx)
	defer cancel()
	return c.Client.Modify(ctx, id, parameters, secrets)
}
