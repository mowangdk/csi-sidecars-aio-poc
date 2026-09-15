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

func libraryFixture() []byte {
	return []byte(`package controller
import("context"; "sync"; "fmt"; "k8s.io/klog/v2")
func (ctrl *ProvisionController) Run(ctx context.Context) {
 run := func(ctx context.Context) {
 ` + libraryMetrics + `
 go ctrl.slowSet.Run(ctx.Done())
 <-ctx.Done()
 }
 go ctrl.volumeStore.Run(ctx, DefaultThreadiness)
 logger := klog.FromContext(ctx)
 ` + libraryElection + `
}
`)
}

func TestLibraryLifecycleClone(t *testing.T) {
	source := libraryFixture()
	before := bytes.Clone(source)
	out, err := cloneProvisionerLibrary(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, before) {
		t.Fatal("changed standalone library")
	}
	for _, removed := range []string{"FlushAndExit", "RunOrDie", "ListenAndServe", "wait.Forever", "panic("} {
		if strings.Contains(string(out), removed) {
			t.Fatalf("retained unsafe path: %s", removed)
		}
	}
	for _, required := range []string{"RunAIO", "ctrl.leaderElection || ctrl.metricsPort != 0", "aioQueueStore", "defer wg.Wait()", "defer cancel()"} {
		if !strings.Contains(string(out), required) {
			t.Fatalf("missing %s", required)
		}
	}
	if strings.Count(string(out), "wg.Go(") != 2 {
		t.Fatalf("unjoined helpers: %s", out)
	}
	for _, mutation := range []string{
		strings.Replace(string(source), "panic(\"unreachable\")", "panic(\"changed\")", 1),
		strings.Replace(string(source), "go wait.Forever", "go unexpected", 1),
		strings.Replace(string(source), "ctrl.leaderElection {", "ctrl.otherElection {", 1),
	} {
		if _, err := cloneProvisionerLibrary([]byte(mutation)); err == nil {
			t.Fatal("accepted unknown library lifecycle")
		}
	}
}

func TestConstructorUsesPrivateSchemeAndReturnsErrors(t *testing.T) {
	source := []byte(`package controller
import("context"; "fmt"; "k8s.io/klog/v2")
func NewProvisionController(ctx context.Context) *ProvisionController {
 if err != nil { logger.Error(err, "hostname"); klog.FlushAndExit(klog.ExitFlushTimeout, 1) }
 v1.AddToScheme(scheme.Scheme)
 broadcaster := record.NewBroadcaster(record.WithContext(ctx))
 recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{})
 if err != nil { logger.Error(err, "options"); klog.FlushAndExit(klog.ExitFlushTimeout, 1) }
 if err != nil { logger.Error(err, "indexer"); klog.FlushAndExit(klog.ExitFlushTimeout, 1) }
 return controller
}`)
	out, err := cloneProvisionerConstructor(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "scheme.Scheme") || strings.Contains(string(out), "FlushAndExit") {
		t.Fatalf("unsafe constructor: %s", out)
	}
	if !strings.Contains(string(out), "broadcaster.Shutdown") {
		t.Fatal("missing event cleanup")
	}
	for _, changed := range []string{
		strings.Replace(string(source), "v1.AddToScheme(scheme.Scheme)", "v1.AddToScheme(otherScheme)", 1),
		strings.Replace(string(source), "record.WithContext(ctx)", "record.WithContext(context.Background())", 1),
	} {
		if _, err := cloneProvisionerConstructor([]byte(changed)); err == nil {
			t.Fatal("accepted unknown constructor lifecycle")
		}
	}
}

func TestGoroutineEvaluationShapes(t *testing.T) {
	for _, tc := range []struct {
		call  string
		valid bool
	}{
		{"ctrl.work(ctx)", true},
		{"wait.Until(func() { ctrl.work(ctx) }, time.Second, ctx.Done())", true},
		{"func() { <-ctx.Done(); queue.ShutDown() }()", true},
		{"ctrl.work(makeContext())", false},
		{"factory().work(ctx)", false},
		{"ctrl.work(<-contexts)", false},
		{"ctrl.work(contexts[index])", false},
		{"func() { go ctrl.work(ctx) }()", false},
		{"ctrl.work(func() { go auxiliary() })", false},
	} {
		t.Run(tc.call, func(t *testing.T) {
			source := []byte("package fixture\nfunc Run() { go " + tc.call + " }")
			out, err := joinMethodGoroutines(source, "Run", "wg")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t: error=%v output=%s", tc.valid, err, out)
			}
		})
	}
}

func TestDependencyCorpus(t *testing.T) {
	root := os.Getenv("CSI_AIO_LIFECYCLE_CORPUS")
	if root == "" {
		t.Skip("set CSI_AIO_LIFECYCLE_CORPUS for dependency corpus")
	}
	root = filepath.Clean(filepath.Join(root, "../..", provisionerLibrary))
	for name, clone := range map[string]func([]byte) ([]byte, error){"controller.go": cloneProvisionerLibrary, "volume_store.go": cloneQueueStore} {
		source, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "controller.go" {
			constructor, err := cloneProvisionerConstructor(source)
			if err != nil {
				t.Fatalf("constructor: %v", err)
			}
			if strings.Contains(string(constructor), "scheme.Scheme") || strings.Contains(string(constructor), "FlushAndExit") {
				t.Fatal("constructor still mutates shared scheme or exits")
			}
		}
		out, err := clone(source)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), "context.Background()") {
			t.Fatalf("detached context in %s", name)
		}
	}
}
