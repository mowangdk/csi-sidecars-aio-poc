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
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const startupFixture = `
ctx := context.Background()
if standardflags.Configuration.ShowVersion {
 fmt.Println(os.Args[0], version)
 os.Exit(0)
}
if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(featureGates); err != nil { klog.Fatal(err) }
csiConn, err := connection.Connect(ctx, address, manager, connection.OnConnectionLoss(connection.ExitOnConnectionLoss()))
if err != nil { klog.Errorf("connection: %v", err); os.Exit(1) }
if addr != "" {
 go func() {
  klog.Infof("listening: %s", addr)
  err := http.ListenAndServe(addr, mux)
  if err != nil { klog.Fatalf("serve: %v", err) }
 }()
}
`

func fixture(sidecar, startup, tail string) []byte {
	return []byte("package main\nimport (\"context\"; \"github.com/kubernetes-csi/csi-lib-utils/leaderelection\")\nfunc " + sidecar + "_main(ctx context.Context) {\n" + startup + tail + "\n}\n")
}

func TestKnownLifecycleFixtures(t *testing.T) {
	for sidecar := range lockNames {
		for index, tail := range expectedTails(sidecar) {
			t.Run(sidecar+string(rune('0'+index)), func(t *testing.T) {
				startup := startupFixture
				if sidecar == "attacher" {
					startup += "connection.SetMaxGRPCLogLength(*maxGRPCLogLength)\n"
				}
				source := fixture(sidecar, startup, tail)
				result, err := adapt(source, sidecar)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parser.ParseFile(token.NewFileSet(), "result.go", result, parser.AllErrors); err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{"os.Exit(", "FlushAndExit(", "klog.Fatal", "signal.Notify(", "SetupSignalHandler(", "context.Background(", "SetFromMap(", "ListenAndServe(", "SetMaxGRPCLogLength("} {
					if strings.Contains(string(result), forbidden) {
						t.Fatalf("retained %s\n%s", forbidden, result)
					}
				}
				for _, required := range []string{"(result error)", "scope.Close(result)", "scope.ConnectionLost", "scope.ServeHTTP(", "workerScope.Close(result)", "wg.Wait()", "return leaderelection.RunWithLeaderElection"} {
					if !strings.Contains(string(result), required) {
						t.Fatalf("missing %s\n%s", required, result)
					}
				}
				if _, err := adapt(result, sidecar); err == nil {
					t.Fatal("accepted already adapted input")
				}
			})
		}
	}
}

func TestRejectUnknownLifecycle(t *testing.T) {
	for sidecar := range lockNames {
		for _, tail := range expectedTails(sidecar) {
			for _, changed := range []string{
				strings.Replace(tail, "ctx, terminate = context.WithCancel(ctx)", "ctx, terminate = context.WithCancel(context.Background())", 1),
				strings.Replace(tail, "run :=", "startAnotherWorker(); run :=", 1),
				tail + "\nnewCleanup()\n",
			} {
				if _, err := adapt(fixture(sidecar, startupFixture, changed), sidecar); err == nil {
					t.Fatalf("%s accepted unknown tail", sidecar)
				}
			}
		}
	}
}

func TestRejectUnknownStartup(t *testing.T) {
	for _, startup := range []string{
		"go backgroundWorker()\n",
		"os.Exit(1)\n",
		"ctx := context.WithValue(context.Background(), key, value)\n",
		"csi.New(ctx, anotherAddress, *timeout, metricsManager)\n",
		"getDriverName(anotherClient, *timeout)\n",
		"owner.Lookup(config, ns)\n",
		"rpc.ProbeForever(ctx)\n",
		"connection.SetMaxGRPCLogLength(otherLimit)\n",
		"if enabled { connection.SetMaxGRPCLogLength(*maxGRPCLogLength) }\n",
		strings.Replace(startupFixture, "err := http.ListenAndServe(addr, mux)", "newSideEffect(); err := http.ListenAndServe(addr, mux)", 1),
	} {
		if _, err := adapt(fixture("attacher", startup, expectedTails("attacher")[0]), "attacher"); err == nil {
			t.Fatalf("accepted unknown startup: %s", startup)
		}
	}
}

func TestHelpersRemainUnchanged(t *testing.T) {
	source := []byte("package main\nfunc helper() { panic(\"unchanged\") }\n")
	result, err := adapt(source, "attacher")
	if err != nil || string(result) != string(source) {
		t.Fatalf("modified helper: %s: %v", result, err)
	}
}

// The optional source corpus complements, but never substitutes for, fixtures.
// It validates real pre-adaptation outputs retained by isolated Linux assembly.
func TestAssembledSourceCorpus(t *testing.T) {
	root := os.Getenv("CSI_AIO_LIFECYCLE_CORPUS")
	if root == "" {
		t.Skip("set CSI_AIO_LIFECYCLE_CORPUS to a pre-adaptation cmd/csi-sidecars directory")
	}
	names := map[string]string{"attacher": "attacher_main.go", "provisioner": "provisioner_csi-provisioner.go", "resizer": "resizer_main.go", "snapshotter": "snapshotter_main.go"}
	for sidecar, name := range names {
		source, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		result, err := adapt(source, sidecar)
		if err != nil {
			t.Errorf("%s: %v", sidecar, err)
		}
		if strings.Contains(string(result), "SetMaxGRPCLogLength(") {
			t.Errorf("%s still mutates shared gRPC logging state", sidecar)
		}
	}
}
