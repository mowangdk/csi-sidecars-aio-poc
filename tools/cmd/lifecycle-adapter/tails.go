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

// These fixtures freeze the lifecycle statements reviewed in the selected
// upstream sources. Whitespace and comments are immaterial; any token change
// requires an explicit compatibility review instead of silently dropping work.
const signalTail = `
var (
 terminate func()
 controllerCtx context.Context
 shutdownHandler <-chan struct{}
)
if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
 ctx, terminate = context.WithCancel(ctx)
 var cancelControllerCtx context.CancelFunc
 controllerCtx, cancelControllerCtx = context.WithCancel(ctx)
 shutdownHandler = server.SetupSignalHandler()
 defer terminate()
 go func() {
  defer cancelControllerCtx()
  <-shutdownHandler
  klog.Info("Received SIGTERM or SIGINT signal, shutting down controller.")
 }()
}
`

const attacherWorker = `
run := func(ctx context.Context) {
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  var wg sync.WaitGroup
  factory.Start(shutdownHandler)
  ctrl.Run(controllerCtx, int(*workerThreads), &wg)
  wg.Wait()
  terminate()
 } else {
  stopCh := ctx.Done()
  factory.Start(stopCh)
  ctrl.Run(ctx, int(*workerThreads), nil)
 }
}
`

const attacherForkElection = `
if !*enableLeaderElection {
 run(klog.NewContext(context.Background(), logger))
} else {
 leClientset, err := kubernetes.NewForConfig(config)
 if err != nil {
  logger.Error(err, "Failed to create leaderelection client")
  klog.FlushAndExit(klog.ExitFlushTimeout, 1)
 }
 lockName := "external-attacher-leader-" + csiAttacher
 le := leaderelection.NewLeaderElection(leClientset, lockName, run)
 if *httpEndpoint != "" {
  le.PrepareHealthCheck(mux, leaderelection.DefaultHealthCheckTimeout)
 }
 if *leaderElectionNamespace != "" { le.WithNamespace(*leaderElectionNamespace) }
 le.WithLeaseDuration(*leaderElectionLeaseDuration)
 le.WithRenewDeadline(*leaderElectionRenewDeadline)
 le.WithRetryPeriod(*leaderElectionRetryPeriod)
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  le.WithReleaseOnCancel(true)
  le.WithContext(ctx)
 }
 if err := le.Run(); err != nil {
  logger.Error(err, "Failed to initialize leader election")
  klog.FlushAndExit(klog.ExitFlushTimeout, 1)
 }
}
`

const resizerWorker = `
run := func(ctx context.Context) {
 informerFactory.Start(ctx.Done())
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  var wg sync.WaitGroup
  if rc != nil { go rc.Run(*workers, controllerCtx, &wg) }
  if mc != nil && utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass) {
   go mc.Run(*workers, controllerCtx, &wg)
  }
  <-controllerCtx.Done()
  wg.Wait()
  terminate()
 } else {
  if rc != nil { go rc.Run(*workers, ctx, nil) }
  if mc != nil && utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass) {
   go mc.Run(*workers, ctx, nil)
  }
  <-ctx.Done()
 }
}
`

const snapshotterWorker = `
run := func(ctx context.Context) {
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  stopCh := controllerCtx.Done()
  snapshotContentfactory.Start(stopCh)
  factory.Start(stopCh)
  coreFactory.Start(stopCh)
  var controllerWg sync.WaitGroup
  go ctrl.Run(*snapshotterThreads, stopCh, &controllerWg)
  <-shutdownHandler
  controllerWg.Wait()
  terminate()
 } else {
  stopCh := make(chan struct{})
  snapshotContentfactory.Start(stopCh)
  factory.Start(stopCh)
  coreFactory.Start(stopCh)
  go ctrl.Run(*snapshotterThreads, stopCh, nil)
  c := make(chan os.Signal, 1)
  signal.Notify(c, os.Interrupt)
  <-c
  close(stopCh)
 }
}
`

const provisionerWorker = `
run := func(ctx context.Context) {
 factory.Start(ctx.Done())
 if factoryForNamespace != nil { factoryForNamespace.Start(ctx.Done()) }
 if topologyInformer != nil { go topologyInformer.RunWorker(ctx) }
 cacheSyncResult := factory.WaitForCacheSync(ctx.Done())
 for _, v := range cacheSyncResult { if !v { klog.Fatalf("Failed to sync Informers!") } }
 if utilfeature.DefaultFeatureGate.Enabled(features.CrossNamespaceVolumeDataSource) {
  if gatewayFactory != nil { gatewayFactory.Start(ctx.Done()) }
  gatewayCacheSyncResult := gatewayFactory.WaitForCacheSync(ctx.Done())
  for _, v := range gatewayCacheSyncResult {
   if !v { klog.Fatalf("Failed to sync Informers for gateway!") }
  }
 }
 if utilfeature.DefaultFeatureGate.Enabled(features.ReleaseLeaderElectionOnExit) {
  var wg sync.WaitGroup
  if capacityController != nil {
   wg.Add(1)
   go func() {
    defer wg.Done()
    capacityController.Run(controllerCtx, int(*capacityThreads), &wg)
   }()
  }
  if csiClaimController != nil {
   wg.Add(1)
   go func() {
    defer wg.Done()
    csiClaimController.Run(controllerCtx, int(*finalizerThreads), &wg)
   }()
  }
  if csiSnapshotFinalizerController != nil {
   wg.Add(1)
   go func() {
    defer wg.Done()
    csiSnapshotFinalizerController.Run(controllerCtx, &wg)
   }()
  }
  provisionController.ControllerWaitGroup(&wg)
  provisionController.Run(controllerCtx)
  wg.Wait()
  terminate()
 } else {
  if capacityController != nil { go capacityController.Run(ctx, int(*capacityThreads), nil) }
  if csiClaimController != nil { go csiClaimController.Run(ctx, int(*finalizerThreads), nil) }
  if csiSnapshotFinalizerController != nil { go csiSnapshotFinalizerController.Run(ctx, nil) }
  provisionController.Run(ctx)
 }
}
`

var lockNames = map[string]string{
	"attacher":    `"external-attacher-leader-" + csiAttacher`,
	"provisioner": `strings.Replace(provisionerName, "/", "-", -1)`,
	"resizer":     `"external-resizer-" + util.SanitizeName(leaseHolder)`,
	"snapshotter": `fmt.Sprintf("%s-%s", snapshotterPrefix, strings.Replace(driverName, "/", "-", -1))`,
}

var workers = map[string]string{
	"attacher":    attacherWorker,
	"provisioner": provisionerWorker,
	"resizer":     resizerWorker,
	"snapshotter": snapshotterWorker,
}
