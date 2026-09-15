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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/client-go/kubernetes/scheme"
)

type lifecycleDriver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedGroupControllerServer
	probe             chan struct{}
	once              sync.Once
	blockProbe        bool
	snapshotStarted   chan struct{}
	snapshotStopped   chan struct{}
	snapshotStartOnce sync.Once
	snapshotStopOnce  sync.Once
}

func (d *lifecycleDriver) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	d.once.Do(func() { close(d.probe) })
	if d.blockProbe {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
func (*lifecycleDriver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: "aio.test.driver", VendorVersion: "test"}, nil
}
func (*lifecycleDriver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
	}}, nil
}
func (*lifecycleDriver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	response := &csi.ControllerGetCapabilitiesResponse{}
	for _, capability := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	} {
		response.Capabilities = append(response.Capabilities, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: capability}}})
	}
	return response, nil
}

func (*lifecycleDriver) GroupControllerGetCapabilities(context.Context, *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{Capabilities: []*csi.GroupControllerServiceCapability{
		{Type: &csi.GroupControllerServiceCapability_Rpc{Rpc: &csi.GroupControllerServiceCapability_RPC{
			Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
		}}},
	}}, nil
}

func (d *lifecycleDriver) ListSnapshots(ctx context.Context, request *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if d.snapshotStarted == nil || request.SnapshotId != "fixture-snapshot" {
		return nil, status.Error(codes.InvalidArgument, "unexpected snapshot request")
	}
	d.snapshotStartOnce.Do(func() { close(d.snapshotStarted) })
	<-ctx.Done()
	d.snapshotStopOnce.Do(func() { close(d.snapshotStopped) })
	return nil, status.FromContextError(ctx.Err()).Err()
}

// lifecycleAPI is an isolated HTTP fixture, not a cluster. It supplies informer
// lists/watches, an optional snapshot event, and resourceVersion-checked leases.
type lifecycleAPI struct {
	mu            sync.Mutex
	leases        map[string]map[string]any
	revision      int
	watches       map[string]bool
	denyRenew     bool
	snapshotEvent <-chan struct{}
}

func (a *lifecycleAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	if strings.Contains(path, "/leases") {
		a.lease(w, r)
		return
	}
	if strings.HasSuffix(path, "/events") && r.Method == http.MethodPost {
		event, err := decodeAPIObject(r)
		if err != nil {
			http.Error(w, "invalid event", 400)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(event)
		return
	}
	resource := path[strings.LastIndex(path, "/")+1:]
	kind := map[string]string{
		"persistentvolumes": "PersistentVolume", "persistentvolumeclaims": "PersistentVolumeClaim",
		"volumeattachments": "VolumeAttachment", "storageclasses": "StorageClass",
		"csinodes": "CSINode", "nodes": "Node", "pods": "Pod",
		"volumesnapshots": "VolumeSnapshot", "volumesnapshotcontents": "VolumeSnapshotContent",
		"volumesnapshotclasses": "VolumeSnapshotClass", "volumegroupsnapshotcontents": "VolumeGroupSnapshotContent",
		"volumegroupsnapshotclasses": "VolumeGroupSnapshotClass", "volumegroupsnapshots": "VolumeGroupSnapshot",
		"referencegrants": "ReferenceGrant", "volumeattributesclasses": "VolumeAttributesClass",
	}[resource]
	if kind == "" {
		// Discovery for optional APIs: advertise no optional resources.
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": strings.TrimPrefix(path, "/apis/"), "resources": []any{}})
		return
	}
	version := "v1"
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 2 && parts[0] == "apis" {
		version = parts[1] + "/" + parts[2]
	}
	if r.URL.Query().Get("watch") == "true" {
		a.mu.Lock()
		a.watches[resource] = true
		a.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("sendInitialEvents") == "true" {
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
				"kind": kind, "apiVersion": version, "metadata": map[string]any{
					"resourceVersion": "1", "annotations": map[string]string{"k8s.io/initial-events-end": "true"},
				},
			}})
		}
		w.(http.Flusher).Flush()
		if resource == "volumesnapshotcontents" && a.snapshotEvent != nil {
			select {
			case <-a.snapshotEvent:
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": map[string]any{
					"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent",
					"metadata": map[string]any{"name": "fixture-content", "uid": "fixture-content-uid", "resourceVersion": "2"},
					"spec": map[string]any{"driver": "aio.test.driver", "deletionPolicy": "Retain",
						"source":            map[string]string{"snapshotHandle": "fixture-snapshot"},
						"volumeSnapshotRef": map[string]string{"name": "fixture", "namespace": "test", "uid": "fixture-uid"}},
				}})
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
		}
		<-r.Context().Done()
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": kind + "List", "apiVersion": version, "metadata": map[string]string{"resourceVersion": "1"}, "items": []any{}})
}
func (a *lifecycleAPI) lease(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.denyRenew {
		http.Error(w, "fixture lease access denied", http.StatusForbidden)
		return
	}
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if r.Method == http.MethodGet {
		if value := a.leases[name]; value != nil {
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
		return
	}
	value, err := decodeAPIObject(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	metadata, ok := value["metadata"].(map[string]any)
	if !ok {
		http.Error(w, "missing metadata", 400)
		return
	}
	name, _ = metadata["name"].(string)
	old := a.leases[name]
	conflict := old != nil && (r.Method == http.MethodPost || metadata["resourceVersion"] != old["metadata"].(map[string]any)["resourceVersion"])
	if conflict {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409})
		return
	}
	a.revision++
	metadata["resourceVersion"] = fmt.Sprint(a.revision)
	value["apiVersion"] = "coordination.k8s.io/v1"
	value["kind"] = "Lease"
	a.leases[name] = value
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(value)
}

// Built-in Kubernetes clients may negotiate protobuf even though watch streams
// and fixture responses use JSON. Decode both without changing production clients.
func decodeAPIObject(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/vnd.kubernetes.protobuf") {
		object, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, nil)
		if err != nil {
			return nil, err
		}
		data, err = json.Marshal(object)
		if err != nil {
			return nil, err
		}
	}
	var value map[string]any
	err = json.Unmarshal(data, &value)
	return value, err
}

// Observe post-cache-sync worker startup, not merely an open informer watch.
// Writer calls may split log lines, so retain partial output until all markers
// are present. Readiness signals are channel-coordinated without sleep polling.
type lifecycleOutput struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	markers []string
	ready   chan struct{}
}

func newLifecycleOutput(controllers []string) *lifecycleOutput {
	markers := map[string]string{
		"attacher":    `worker="CSIAttachController"`,
		"provisioner": `"Started provisioner controller"`,
		"resizer":     `worker="resizeController"`,
		"snapshotter": `worker="csiSnapshotSideCarController"`,
	}
	out := &lifecycleOutput{ready: make(chan struct{})}
	for _, controller := range controllers {
		out.markers = append(out.markers, markers[controller])
	}
	return out
}
func (o *lifecycleOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buffer.Write(p)
	if o.markers != nil {
		for _, marker := range o.markers {
			if !strings.Contains(o.buffer.String(), marker) {
				return n, err
			}
		}
		o.markers = nil
		close(o.ready)
	}
	return n, err
}
func (o *lifecycleOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

func TestAssembledControllerCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	for _, controller := range []string{"attacher", "provisioner", "resizer", "snapshotter", "attacher,provisioner,resizer,snapshotter"} {
		for _, election := range []bool{false, true} {
			for _, duringProbe := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/election=%t/probe=%t", controller, election, duringProbe), func(t *testing.T) {
					runAssembledLifecycle(t, controller, election, duringProbe, "signal")
				})
			}
		}
	}
}

func TestAssembledFailureCancelsAllControllers(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	for _, failure := range []string{"connection", "lease"} {
		t.Run(failure, func(t *testing.T) {
			runAssembledLifecycle(t, "attacher,provisioner,resizer,snapshotter", true, false, failure)
		})
	}
}

func TestAssembledSnapshotOperationCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	for _, election := range []bool{false, true} {
		t.Run(fmt.Sprintf("election=%t", election), func(t *testing.T) {
			runAssembledLifecycle(t, "attacher,provisioner,resizer,snapshotter", election, false, "snapshot-signal")
		})
	}
}

func TestAssembledGRPCLogging(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	for _, controllers := range []string{"attacher", "provisioner", "attacher,provisioner,resizer,snapshotter"} {
		t.Run(controllers, func(t *testing.T) {
			output := runAssembledLifecycle(t, controllers, false, false, "signal", "--v=5", "--attacher-max-grpc-log-length=16")
			if !strings.Contains(output, "GRPC response") {
				t.Fatalf("verbose RPC logging was not exercised\n%s", output)
			}
			wantLimit := strings.Contains(controllers, "attacher")
			if strings.Contains(output, "log capped to 16 chars") != wantLimit {
				t.Fatalf("attacher-enabled log limit=%t was not preserved\n%s", wantLimit, output)
			}
		})
	}
}

func runAssembledLifecycle(t *testing.T, controllers string, election, duringProbe bool, stop string, extraArgs ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	driver := &lifecycleDriver{probe: make(chan struct{}), blockProbe: duringProbe}
	if stop == "snapshot-signal" {
		driver.snapshotStarted, driver.snapshotStopped = make(chan struct{}), make(chan struct{})
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "csi.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, driver)
	csi.RegisterControllerServer(server, driver)
	csi.RegisterGroupControllerServer(server, driver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	api := &lifecycleAPI{leases: map[string]map[string]any{}, watches: map[string]bool{}}
	var snapshotEvent chan struct{}
	if stop == "connection" || stop == "snapshot-signal" {
		snapshotEvent = make(chan struct{})
		api.snapshotEvent = snapshotEvent
	}
	httpServer := httptest.NewServer(api)
	defer httpServer.Close()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", httpServer.URL)
	if err := os.WriteFile(kubeconfig, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestCLIProcess$", "--", "--controllers=" + controllers,
		"--kubeconfig=" + kubeconfig, "--csi-address=unix://" + listener.Addr().String(),
		fmt.Sprintf("--leader-election=%t", election), "--leader-election-namespace=test",
		"--leader-election-lease-duration=3s", "--leader-election-renew-deadline=2s",
		"--leader-election-retry-period=500ms", "--shutdown-timeout=3s", "--v=2"}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), "CSI_AIO_CLI_HELPER=1", "KUBECONFIG="+kubeconfig)
	output := newLifecycleOutput(strings.Split(controllers, ","))
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	running := true
	defer func() {
		cancel()
		if running {
			<-done
		}
	}()
	ready := driver.probe
	if !duringProbe {
		ready = output.ready
	}
	select {
	case <-ready:
	case err := <-done:
		running = false
		t.Fatalf("runner exited before readiness: %v\n%s", err, output)
	case <-ctx.Done():
		t.Fatalf("runner initialization timed out\n%s", output)
	}
	if !duringProbe {
		api.mu.Lock()
		leaseCount, groupWatch := len(api.leases), api.watches["volumegroupsnapshotcontents"]
		api.mu.Unlock()
		if election && leaseCount != len(strings.Split(controllers, ",")) {
			t.Fatalf("expected independent controller leases, got %d\n%s", leaseCount, output)
		}
		if strings.Contains(controllers, "snapshotter") && !groupWatch {
			t.Fatalf("group snapshot workers were not exercised\n%s", output)
		}
	}
	if stop == "snapshot-signal" {
		close(snapshotEvent)
		select {
		case <-driver.snapshotStarted:
		case <-ctx.Done():
			t.Fatalf("snapshot reconciliation did not reach CSI\n%s", output)
		}
	}
	wantFailure := ""
	switch stop {
	case "signal", "snapshot-signal":
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	case "connection":
		wantFailure = "snapshotter: lost connection to CSI driver"
		server.Stop()
		// The upstream callback detects loss on a subsequent dial. Deliver a
		// real snapshot event after disconnect to exercise that runner's RPC.
		close(snapshotEvent)
	case "lease":
		wantFailure = "leadership lost"
		api.mu.Lock()
		api.denyRenew = true
		api.mu.Unlock()
	default:
		t.Fatalf("unknown stop trigger %q", stop)
	}
	err = <-done
	running = false
	if ctx.Err() != nil || (err == nil) != (wantFailure == "") || !strings.Contains(output.String(), wantFailure) {
		t.Fatalf("shutdown exit=%v, context=%v, want=%q\n%s", err, ctx.Err(), wantFailure, output)
	}
	if stop == "snapshot-signal" {
		select {
		case <-driver.snapshotStopped:
		case <-ctx.Done():
			t.Fatal("snapshot RPC remained active after runner shutdown")
		}
	}
	if strings.Contains(output.String(), "panic:") || strings.Contains(output.String(), "shutdown deadline exceeded") {
		t.Fatalf("unsafe shutdown\n%s", output)
	}
	return output.String()
}
