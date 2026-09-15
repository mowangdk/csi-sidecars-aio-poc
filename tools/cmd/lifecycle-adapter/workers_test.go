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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workerFixture = `package controller
import("context";"sync";"unused")
func (ctrl *controller) Run(ctx context.Context, wg *sync.WaitGroup) {
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  wg.Add(1)
  go func() { defer wg.Done(); ctrl.work(ctx) }()
 } else {
  go ctrl.work(ctx)
 }
 go ctrl.auxiliary(ctx)
 <-ctx.Done()
}
`

func TestWorkerCloneTracksAuxiliaryAndRetainsOriginal(t *testing.T) {
	source := []byte(workerFixture)
	original := bytes.Clone(source)
	result, err := cloneWorker(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) {
		t.Fatal("mutated standalone input")
	}
	for _, absent := range []string{"ReleaseLeaderElectionOnExit", "unused", "func (ctrl *controller) Run("} {
		if strings.Contains(string(result), absent) {
			t.Fatalf("unexpected %s in %s", absent, result)
		}
	}
	if strings.Count(string(result), "wg.Go(") != 2 || !strings.Contains(string(result), "RunAIO(") {
		t.Fatalf("untracked worker: %s", result)
	}
}

func TestUnknownWorkerShapeFails(t *testing.T) {
	for _, source := range []string{
		strings.Replace(workerFixture, "wg *sync.WaitGroup", "wg any", 1),
		strings.Replace(workerFixture, "ReleaseLeaderElectionOnExit", "OtherGate", 1),
		strings.Replace(workerFixture, "ctrl.auxiliary(ctx)", "ctrl.auxiliary(makeContext())", 1),
	} {
		if _, err := cloneWorker([]byte(source)); err == nil {
			t.Fatal("accepted unknown worker shape")
		}
	}
}

func TestTopologyWorkersAreJoinedAndFixedTopologyWaits(t *testing.T) {
	node := []byte("package topology\nimport \"context\"\n" + topologyWorker)
	fixed := []byte("package topology\nfunc (mt *Mock) RunWorker(ctx context.Context) {}")
	out, err := cloneTopology(node, fixed)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"defer wg.Wait()", "defer cancel()", "wg.Go(", "case *Mock:", "<-ctx.Done()"} {
		if !strings.Contains(string(out), required) {
			t.Fatalf("missing %s", required)
		}
	}
	if _, err := cloneTopology(node, []byte(strings.Replace(string(fixed), "{}", "{ go work() }", 1))); err == nil {
		t.Fatal("accepted active fixed topology")
	}
}

func TestSnapshotHandlerLifecycleFixture(t *testing.T) {
	prefix := `package controller
import ("context"; "time")
func NewCSIHandler(a T, b T, c T, d T, e T, f T, g T) Handler { return &csiHandler{} }
`
	var methods strings.Builder
	for _, name := range snapshotOperations {
		methods.WriteString("func (handler *csiHandler) " + name + "() { ctx, cancel := context.WithTimeout(context.Background(), handler.timeout); defer cancel(); work(ctx) }\n")
	}
	source := []byte(prefix + methods.String())
	original := bytes.Clone(source)
	out, err := cloneSnapshotHandler(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) {
		t.Fatal("mutated standalone snapshot handler")
	}
	if strings.Contains(string(out), "context.Background()") || strings.Count(string(out), "context.WithTimeout(handler.ctx, handler.timeout)") != 6 {
		t.Fatalf("unowned RPCs: %s", out)
	}
	for _, changed := range []string{
		strings.Replace(string(source), "context.Background()", "context.TODO()", 1),
		strings.Replace(string(source), "defer cancel()", "defer otherCancel()", 1),
		strings.Replace(string(source), "&csiHandler{}", "&otherHandler{}", 1),
		strings.Replace(string(source), "work(ctx)", "go work(ctx)", 1),
		strings.Replace(string(source), "work(ctx)", "work(context.Background())", 1),
		strings.Replace(string(source), "work(ctx)", "work(context.WithoutCancel(ctx))", 1),
		strings.Replace(string(source), "work(ctx)", "klog.Fatal(\"failed\")", 1),
		strings.Replace(string(source), "work(ctx)", "signal.Notify(signals)", 1),
	} {
		if _, err := cloneSnapshotHandler([]byte(changed)); err == nil {
			t.Fatal("accepted unknown snapshot handler")
		}
	}
}

func TestSnapshotConstructorLifecycleFixture(t *testing.T) {
	params := make([]string, 19)
	for i := range params {
		params[i] = string(rune('a'+i)) + " T"
	}
	source := []byte(`package controller
import ("k8s.io/api/core/v1"; "k8s.io/client-go/kubernetes/scheme")
func NewCSISnapshotSideCarController(` + strings.Join(params, ", ") + `) *csiSnapshotSideCarController {
 broadcaster := record.NewBroadcaster()
 recorder := broadcaster.NewRecorder(scheme.Scheme, source)
 handler := NewCSIHandler(a, b, c, d, e, f, g)
 ctrl := &csiSnapshotSideCarController{recorder: recorder, handler: handler}
 return ctrl
}
`)
	original := bytes.Clone(source)
	out, err := cloneSnapshotConstructor(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) {
		t.Fatal("mutated standalone snapshot constructor")
	}
	for _, required := range []string{"eventScheme := k8sruntime.NewScheme()", "v1.AddToScheme(eventScheme)", "snapshotscheme.AddToScheme(eventScheme)", "record.WithContext(ctx)", "NewCSIHandlerAIO(ctx,", "return ctrl, broadcaster.Shutdown, nil"} {
		if !strings.Contains(string(out), required) {
			t.Fatalf("missing %s in %s", required, out)
		}
	}
	for _, changed := range []string{
		strings.Replace(string(source), "record.NewBroadcaster()", "record.NewBroadcaster(otherOption)", 1),
		strings.Replace(string(source), "scheme.Scheme", "otherScheme", 1),
		strings.Replace(string(source), "NewCSIHandler(a, b, c, d, e, f, g)", "NewCSIHandler(a)", 1),
		strings.Replace(string(source), "return ctrl", "return otherController", 1),
		strings.Replace(string(source), "return ctrl", "if failed { return nil }; return ctrl", 1),
		strings.Replace(string(source), "return ctrl", "go work(); return ctrl", 1),
		strings.Replace(string(source), "return ctrl", "work(context.TODO()); return ctrl", 1),
		strings.Replace(string(source), "return ctrl", "os.Exit(1); return ctrl", 1),
		strings.Replace(string(source), "return ctrl", "signal.Notify(signals); return ctrl", 1),
	} {
		if _, err := cloneSnapshotConstructor([]byte(changed)); err == nil {
			t.Fatal("accepted unknown snapshot constructor")
		}
	}
}

func TestAssembledWorkerCorpus(t *testing.T) {
	root := os.Getenv("CSI_AIO_LIFECYCLE_CORPUS")
	if root == "" {
		t.Skip("set CSI_AIO_LIFECYCLE_CORPUS for assembled worker corpus")
	}
	root = filepath.Clean(filepath.Join(root, "../.."))
	if _, err := snapshotOutputs(root); err != nil {
		t.Fatal(err)
	}
	nodes, err := os.ReadFile(filepath.Join(root, "pkg/provisioner/pkg/capacity/topology/nodes.go"))
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := os.ReadFile(filepath.Join(root, "pkg/provisioner/pkg/capacity/topology/mock.go"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cloneTopology(nodes, fixed); err != nil {
		t.Fatal(err)
	}
	for _, path := range workerSources {
		source, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cloneWorker(source); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
