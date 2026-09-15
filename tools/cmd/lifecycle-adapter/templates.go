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

import "strings"

const workerStart = `
run := func(ctx context.Context) (result error) {
 workerScope := aioruntime.NewScope(ctx)
 ctx = workerScope.Context()
 var wg sync.WaitGroup
`

// Close joins the Run goroutines before Wait, so an upstream Run's initial Add
// cannot race a zero-count Wait. Informer Shutdown follows cancellation/drain.
const workerDrain = `
 defer func() {
  result = workerScope.Close(result)
  wg.Wait()
 }()
`

var adaptedWorkers = map[string]string{
	"attacher": workerStart + `
 defer factory.Shutdown()
` + workerDrain + `
 factory.Start(ctx.Done())
 ctrl.RunAIO(ctx, int(*workerThreads), &wg)
 if ctx.Err() == nil { return aioruntime.ErrUnexpectedExit }
 return nil
}
`,
	"snapshotter": workerStart + `
 defer snapshotContentfactory.Shutdown()
 defer factory.Shutdown()
 defer coreFactory.Shutdown()
` + workerDrain + `
 snapshotContentfactory.Start(ctx.Done())
 factory.Start(ctx.Done())
 coreFactory.Start(ctx.Done())
 ctrl.RunAIO(*snapshotterThreads, ctx.Done(), &wg)
 if ctx.Err() == nil { return aioruntime.ErrUnexpectedExit }
 return nil
}
`,
	"resizer": workerStart + `
 defer informerFactory.Shutdown()
` + workerDrain + `
 informerFactory.Start(ctx.Done())
 if rc != nil {
  workerScope.Go("resize", func(ctx context.Context) error {
   return aioruntime.RunWorkerController(rc, *workers, ctx, &wg)
  })
 }
 if mc != nil && utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass) {
  workerScope.Go("modify", func(ctx context.Context) error {
   return aioruntime.RunWorkerController(mc, *workers, ctx, &wg)
  })
 }
 <-ctx.Done()
 return nil
}
`,
	"provisioner": workerStart + `
 defer factory.Shutdown()
 if factoryForNamespace != nil { defer factoryForNamespace.Shutdown() }
 if gatewayFactory != nil { defer gatewayFactory.Shutdown() }
` + workerDrain + `
 factory.Start(ctx.Done())
 if factoryForNamespace != nil { factoryForNamespace.Start(ctx.Done()) }
 if topologyInformer != nil {
  workerScope.Go("topology", func(ctx context.Context) error {
   return topology.RunWorkerAIO(ctx, topologyInformer)
  })
 }
 for _, synced := range factory.WaitForCacheSync(ctx.Done()) {
  if !synced {
   if ctx.Err() != nil { return ctx.Err() }
   return fmt.Errorf("failed to sync informers")
  }
 }
 if utilfeature.DefaultFeatureGate.Enabled(features.CrossNamespaceVolumeDataSource) && gatewayFactory != nil {
  gatewayFactory.Start(ctx.Done())
  for _, synced := range gatewayFactory.WaitForCacheSync(ctx.Done()) {
   if !synced {
    if ctx.Err() != nil { return ctx.Err() }
    return fmt.Errorf("failed to sync gateway informers")
   }
  }
 }
 if capacityController != nil {
  workerScope.Go("capacity", func(ctx context.Context) error {
   capacityController.RunAIO(ctx, int(*capacityThreads), &wg)
   return nil
  })
 }
 if csiClaimController != nil {
  workerScope.Go("cloning-protection", func(ctx context.Context) error {
   csiClaimController.RunAIO(ctx, int(*finalizerThreads), &wg)
   return nil
  })
 }
 if csiSnapshotFinalizerController != nil {
  workerScope.Go("snapshot-finalizer", func(ctx context.Context) error {
   csiSnapshotFinalizerController.RunAIO(ctx, &wg)
   return nil
  })
 }
 if err := provisionController.RunAIO(ctx); err != nil { return err }
 if ctx.Err() == nil { return aioruntime.ErrUnexpectedExit }
 return nil
}
`,
}

func electionCall(sidecar string) string {
	return `leaderelection.RunWithLeaderElection(ctx, config, standardflags.Configuration, run, ` + lockNames[sidecar] + `, mux, utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit))`
}

func expectedTails(sidecar string) []string {
	signals := signalTail
	if sidecar == "attacher" {
		signals = strings.Replace(signals, "klog.Info(", "logger.Info(", 1)
	}
	tails := []string{signals + workers[sidecar] + electionCall(sidecar)}
	if sidecar == "attacher" {
		tails = append(tails, signals+attacherWorker+attacherForkElection)
	}
	return tails
}
