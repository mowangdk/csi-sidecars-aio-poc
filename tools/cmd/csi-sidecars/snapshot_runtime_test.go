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
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	groupv1 "github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/client/apis/volumegroupsnapshot/v1"
	snapshotv1 "github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/client/apis/volumesnapshot/v1"
	controller "github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/pkg/sidecar-controller"
)

type snapshotRequest struct {
	operation, name, id string
	ids                 []string
	parameters, secrets map[string]string
}

type snapshotBackend struct {
	call func(context.Context, snapshotRequest) error
}

func (b snapshotBackend) CreateSnapshot(ctx context.Context, name, volume string, parameters, secrets map[string]string) (string, string, time.Time, int64, bool, error) {
	err := b.call(ctx, snapshotRequest{operation: "create", name: name, id: volume, parameters: parameters, secrets: secrets})
	return "driver", "snapshot-result", time.Unix(123, 0), 42, true, err
}
func (b snapshotBackend) DeleteSnapshot(ctx context.Context, id string, secrets map[string]string) error {
	return b.call(ctx, snapshotRequest{operation: "delete", id: id, secrets: secrets})
}
func (b snapshotBackend) GetSnapshotStatus(ctx context.Context, id string, secrets map[string]string) (bool, time.Time, int64, string, error) {
	err := b.call(ctx, snapshotRequest{operation: "status", id: id, secrets: secrets})
	return true, time.Unix(123, 0), 42, "group-result", err
}
func (b snapshotBackend) CreateGroupSnapshot(ctx context.Context, name string, ids []string, parameters, secrets map[string]string) (string, string, []*csi.Snapshot, time.Time, bool, error) {
	err := b.call(ctx, snapshotRequest{operation: "group-create", name: name, ids: ids, parameters: parameters, secrets: secrets})
	return "driver", "group-result", []*csi.Snapshot{{SnapshotId: "snapshot-result"}}, time.Unix(123, 0), true, err
}
func (b snapshotBackend) DeleteGroupSnapshot(ctx context.Context, id string, ids []string, secrets map[string]string) error {
	return b.call(ctx, snapshotRequest{operation: "group-delete", id: id, ids: ids, secrets: secrets})
}
func (b snapshotBackend) GetGroupSnapshotStatus(ctx context.Context, id string, ids []string, secrets map[string]string) (bool, time.Time, error) {
	err := b.call(ctx, snapshotRequest{operation: "group-status", id: id, ids: ids, secrets: secrets})
	return true, time.Unix(123, 0), err
}

// Exercise the generated methods themselves, including argument mapping and
// deadlines. Standalone construction must retain its independent RPC lifetime.
func TestSnapshotHandlerRuntimeContract(t *testing.T) {
	volume, snapshotID := "volume", "snapshot"
	create := &snapshotv1.VolumeSnapshotContent{}
	create.Spec.VolumeSnapshotRef.UID = "snapshot-uid"
	create.Spec.Source.VolumeHandle = &volume
	existing := &snapshotv1.VolumeSnapshotContent{}
	existing.Spec.Source.SnapshotHandle = &snapshotID
	groupCreate := &groupv1.VolumeGroupSnapshotContent{}
	groupCreate.Spec.VolumeGroupSnapshotRef.UID = "group-uid"
	groupCreate.Spec.Source.VolumeHandles = []string{"volume-a", "volume-b"}
	groupExisting := &groupv1.VolumeGroupSnapshotContent{}
	groupExisting.Spec.Source.GroupSnapshotHandles = &groupv1.GroupSnapshotHandles{VolumeGroupSnapshotHandle: "group"}
	ids := []string{"snapshot-a", "snapshot-b"}
	parameters := map[string]string{"parameter": "value"}
	secrets := map[string]string{"fixture": "not-a-credential"}
	cases := []struct {
		request snapshotRequest
		call    func(controller.Handler) (any, error)
		result  any
	}{
		{snapshotRequest{operation: "create", name: "snap-snapshot-uid", id: volume, parameters: parameters, secrets: secrets},
			func(h controller.Handler) (any, error) {
				d, id, ts, size, ready, err := h.CreateSnapshot(create, parameters, secrets)
				return []any{d, id, ts, size, ready}, err
			},
			[]any{"driver", "snapshot-result", time.Unix(123, 0), int64(42), true}},
		{snapshotRequest{operation: "delete", id: snapshotID, secrets: secrets},
			func(h controller.Handler) (any, error) { return nil, h.DeleteSnapshot(existing, secrets) }, nil},
		{snapshotRequest{operation: "status", id: snapshotID, secrets: secrets},
			func(h controller.Handler) (any, error) {
				ready, ts, size, group, err := h.GetSnapshotStatus(existing, secrets)
				return []any{ready, ts, size, group}, err
			},
			[]any{true, time.Unix(123, 0), int64(42), "group-result"}},
		{snapshotRequest{operation: "group-create", name: "group-group-uid", ids: groupCreate.Spec.Source.VolumeHandles, parameters: parameters, secrets: secrets},
			func(h controller.Handler) (any, error) {
				d, id, snapshots, ts, ready, err := h.CreateGroupSnapshot(groupCreate, parameters, secrets)
				return []any{d, id, snapshots, ts, ready}, err
			},
			[]any{"driver", "group-result", []*csi.Snapshot{{SnapshotId: "snapshot-result"}}, time.Unix(123, 0), true}},
		{snapshotRequest{operation: "group-delete", id: "group", ids: ids, secrets: secrets},
			func(h controller.Handler) (any, error) {
				return nil, h.DeleteGroupSnapshot(groupExisting, ids, secrets)
			}, nil},
		{snapshotRequest{operation: "group-status", id: "group", ids: ids, secrets: secrets},
			func(h controller.Handler) (any, error) {
				ready, ts, err := h.GetGroupSnapshotStatus(groupExisting, ids, secrets)
				return []any{ready, ts}, err
			},
			[]any{true, time.Unix(123, 0)}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"success", "cancel", "deadline", "standalone"} {
			t.Run(tc.request.operation+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parent, cancel := context.WithCancel(context.Background())
					defer cancel()
					started := make(chan context.Context, 1)
					backend := snapshotBackend{call: func(ctx context.Context, request snapshotRequest) error {
						if !reflect.DeepEqual(request, tc.request) {
							t.Errorf("request=%+v, want=%+v", request, tc.request)
						}
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) != 7*time.Second {
							t.Error("lost configured RPC deadline")
						}
						started <- ctx
						if mode == "success" {
							return nil
						}
						<-ctx.Done()
						return ctx.Err()
					}}
					handler := controller.NewCSIHandlerAIO(parent, backend, backend, 7*time.Second, "snap", -1, "group", -1)
					if mode == "standalone" {
						handler = controller.NewCSIHandler(backend, backend, 7*time.Second, "snap", -1, "group", -1)
					}
					done := make(chan error, 1)
					go func() {
						result, err := tc.call(handler)
						if mode == "success" && !reflect.DeepEqual(result, tc.result) {
							t.Errorf("result=%v, want=%v", result, tc.result)
						}
						done <- err
					}()
					request := <-started
					if mode == "cancel" || mode == "standalone" {
						cancel()
					}
					if mode == "standalone" && request.Err() != nil {
						t.Fatal("AIO cancellation affected standalone handler")
					}
					err := <-done
					switch mode {
					case "success":
						if err != nil {
							t.Fatal(err)
						}
					case "cancel":
						if request.Err() != context.Canceled || err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
							t.Fatalf("unowned cancellation: %v", err)
						}
					default:
						if request.Err() != context.DeadlineExceeded || err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
							t.Fatalf("wrong timeout: %v", err)
						}
					}
				})
			})
		}
	}
}
