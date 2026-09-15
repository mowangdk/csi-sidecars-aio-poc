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

// Package leaderelection implements AIO-only drain-before-release election.
// Standalone sidecars continue to use the unmodified upstream election wrapper.
package leaderelection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	aioruntime "github.com/kubernetes-csi/csi-sidecars/pkg/runtime"
	clientleaderelection "k8s.io/client-go/tools/leaderelection"
)

var ErrLeadershipLost = errors.New("leadership lost")

// Run retains the lease while workers drain. Only after worker completion is
// election canceled, honoring config.ReleaseOnCancel. A follower does not wait
// for OnStartedLeading, which client-go might never invoke. The callbacks are
// owned by this adapter; only an optional OnNewLeader observer is preserved.
// The enclosing supervisor owns the process deadline; timeout must not cause an
// early lease release while workers could still be issuing CSI operations.
func Run(parent context.Context, config clientleaderelection.LeaderElectionConfig, worker aioruntime.Runner) error {
	if worker == nil {
		return fmt.Errorf("leader worker must not be nil")
	}
	if parent.Err() != nil {
		return nil
	}
	releaseOnExit := config.ReleaseOnCancel
	// client-go also releases after renewal failure, before OnStoppedLeading.
	// AIO must drain workers first even on that path, so release explicitly.
	config.ReleaseOnCancel = false
	electionCtx, cancelElection := context.WithCancel(context.WithoutCancel(parent))
	defer cancelElection()
	workerCtx, cancelWorker := context.WithCancel(context.WithoutCancel(parent))
	defer cancelWorker()

	var mu sync.Mutex
	var started, stopping, stoppingElection bool
	var leadershipError error
	workerDone := make(chan error, 1)
	electionDone := make(chan struct{})
	config.Callbacks = clientleaderelection.LeaderCallbacks{
		OnNewLeader: config.Callbacks.OnNewLeader,
		OnStartedLeading: func(context.Context) {
			mu.Lock()
			if stopping || parent.Err() != nil {
				mu.Unlock()
				return
			}
			started = true
			mu.Unlock()
			err := worker(workerCtx)
			if err == nil && workerCtx.Err() == nil {
				err = aioruntime.ErrUnexpectedExit
			} else if workerCtx.Err() != nil && err == workerCtx.Err() {
				err = nil
			}
			if err != nil {
				aioruntime.ReportFailure(parent, err)
			}
			workerDone <- err
		},
		OnStoppedLeading: func() {
			mu.Lock()
			unexpected := !stoppingElection
			if unexpected {
				leadershipError = ErrLeadershipLost
				stopping = true
			}
			mu.Unlock()
			if unexpected {
				cancelWorker()
				aioruntime.ReportFailure(parent, ErrLeadershipLost)
			}
		},
	}
	elector, err := clientleaderelection.NewLeaderElector(config)
	if err != nil {
		return fmt.Errorf("initialize leader election: %w", err)
	}
	if config.WatchDog != nil {
		config.WatchDog.SetLeaderElection(elector)
	}
	go func() {
		defer close(electionDone)
		elector.Run(electionCtx)
	}()

	var workerError error
	workerFinished := false
	select {
	case <-parent.Done():
	case workerError = <-workerDone:
		workerFinished = true
	case <-electionDone:
	}

	mu.Lock()
	stopping = true
	wasStarted := started
	mu.Unlock()
	cancelWorker()
	if wasStarted && !workerFinished {
		workerError = <-workerDone
	}

	mu.Lock()
	stoppingElection = true
	mu.Unlock()
	cancelElection()
	<-electionDone
	mu.Lock()
	err = leadershipError
	mu.Unlock()
	if releaseOnExit && err == nil && elector.IsLeader() {
		err = release(parent, config)
	}
	return errors.Join(err, workerError)
}

func release(parent context.Context, config clientleaderelection.LeaderElectionConfig) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), config.RenewDeadline)
	defer cancel()
	record, _, err := config.Lock.Get(ctx)
	if err != nil {
		return fmt.Errorf("read lease before release: %w", err)
	}
	if record.HolderIdentity != config.Lock.Identity() {
		return fmt.Errorf("%w: lease holder changed before release", ErrLeadershipLost)
	}
	now := metav1.NewTime(time.Now())
	if err := config.Lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions:    record.LeaderTransitions,
		LeaseDurationSeconds: 1,
		AcquireTime:          now,
		RenewTime:            now,
	}); err != nil {
		return fmt.Errorf("release drained lease: %w", err)
	}
	return nil
}
