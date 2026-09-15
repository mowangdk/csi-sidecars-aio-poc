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

// Package csistartup provides context-owned AIO startup RPCs. The standalone
// upstream helpers and their process lifecycle remain unchanged.
package csistartup

import (
	"context"
	"errors"
	"fmt"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// Error suppresses only errors caused by this runner's requested cancellation.
// In particular, a joined independent failure must survive shutdown.
func Error(ctx context.Context, message string, cause error) error {
	if ctx.Err() != nil && cancellationOnly(cause, ctx.Err()) {
		return ctx.Err()
	}
	if cause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, cause)
}

func cancellationOnly(err, cancellation error) bool {
	if err == cancellation {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cancellationOnly(child, cancellation) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cancellationOnly(wrapped, cancellation)
	}
	if grpcError, ok := err.(interface{ GRPCStatus() *status.Status }); ok {
		code := grpcError.GRPCStatus().Code()
		return code == codes.Canceled && cancellation == context.Canceled ||
			code == codes.DeadlineExceeded && cancellation == context.DeadlineExceeded
	}
	return false
}

// ProbeForever retains the upstream one-second polling and per-probe deadline,
// but preserves error identity so requested cancellation is distinguishable from
// a driver failure racing shutdown. The upstream helper formats errors with %s.
func ProbeForever(ctx context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	return probeForever(ctx, timeout, func(ctx context.Context) (bool, error) { return rpc.Probe(ctx, conn) })
}

func probeForever(ctx context.Context, timeout time.Duration, probe func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		klog.FromContext(ctx).Info("Probing CSI driver for readiness")
		request, cancel := context.WithTimeout(ctx, timeout)
		ready, err := probe(request)
		cancel()
		if err != nil && !cancellationOnly(err, context.DeadlineExceeded) {
			return Error(ctx, "CSI driver probe failed", err)
		}
		if err == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func DriverName(parent context.Context, get func(context.Context) (string, error), timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return get(ctx)
}

func GetDriverName(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) (string, error) {
	return DriverName(parent, func(ctx context.Context) (string, error) { return rpc.GetDriverName(ctx, conn) }, timeout)
}

func GetDriverCapabilities(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) (rpc.PluginCapabilitySet, rpc.ControllerCapabilitySet, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	plugin, err := rpc.GetPluginCapabilities(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel = context.WithTimeout(parent, timeout)
	defer cancel()
	controller, err := rpc.GetControllerCapabilities(ctx, conn)
	return plugin, controller, err
}

func GetNodeInfo(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) (*csi.NodeGetInfoResponse, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return csi.NewNodeClient(conn).NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
}
