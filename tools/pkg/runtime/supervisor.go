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

// Package runtime provides process supervision for embedded CSI controllers.
// It never exits the process or recovers controller panics.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runner must not return until its workers and resources have stopped.
// Cancellation is a normal return only after the provided context is canceled.
// A runner must propagate failures even if cancellation happens concurrently.
type Runner func(context.Context) error

var (
	ErrUnexpectedExit  = errors.New("runner stopped unexpectedly")
	ErrShutdownTimeout = errors.New("shutdown deadline exceeded")
	ErrForcedShutdown  = errors.New("second signal received during shutdown")
)

type result struct {
	name     string
	err      error
	finished bool
}

type failureReporterKey struct{}

// ReportFailure reports an asynchronous failure before the runner finishes
// draining. The supervisor cancels peers and starts its deadline immediately;
// it still waits for this runner to return. Only the first report per runner is
// sent. Additional errors must be included in the runner's return value.
func ReportFailure(ctx context.Context, err error) {
	if report, ok := ctx.Value(failureReporterKey{}).(func(error)); ok && err != nil {
		report(err)
	}
}

// Supervise cancels all peers on the first failure, waits for drain, and returns
// attributed errors. A shutdown deadline is started only when draining begins.
// Timeout does not release leases or pretend an unfinished runner has stopped.
func Supervise(parent context.Context, timeout time.Duration, runners map[string]Runner) error {
	if timeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive")
	}
	if len(runners) == 0 {
		return fmt.Errorf("no runners configured")
	}
	names := make([]string, 0, len(runners))
	for name, run := range runners {
		if name == "" || run == nil {
			return fmt.Errorf("invalid runner %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if parent.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan result, 2*len(runners))
	pending := make(map[string]bool, len(runners))
	for _, name := range names {
		pending[name] = true
		run := runners[name]
		var reportOnce sync.Once
		runnerCtx := context.WithValue(ctx, failureReporterKey{}, func(err error) {
			reportOnce.Do(func() { results <- result{name: name, err: err} })
		})
		go func() {
			err := run(runnerCtx)
			// Classify at the point of return, not later when the supervisor
			// may already have canceled peers. Never discard wrapped/joined
			// errors that could contain an independent failure.
			if ctx.Err() == nil && err == nil {
				err = ErrUnexpectedExit
			} else if ctx.Err() != nil && err == ctx.Err() {
				err = nil
			}
			results <- result{name: name, err: err, finished: true}
		}()
	}

	var failures []error
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	startDrain := func() {
		if timer == nil {
			timer = time.NewTimer(timeout)
			deadline = timer.C
			cancel()
		}
	}
	stop := parent.Done()
	for len(pending) != 0 {
		select {
		case <-stop:
			startDrain()
			stop = nil
		case outcome := <-results:
			if outcome.finished {
				delete(pending, outcome.name)
			}
			if outcome.err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", outcome.name, outcome.err))
				startDrain()
			}
		case <-deadline:
			var unfinished []string
			for name := range pending {
				unfinished = append(unfinished, name)
			}
			sort.Strings(unfinished)
			failures = append(failures, fmt.Errorf("%w: waiting for %s", ErrShutdownTimeout, strings.Join(unfinished, ", ")))
			return errors.Join(failures...)
		}
	}
	return errors.Join(failures...)
}

// SuperviseSignals consumes notifications registered by the process entrypoint.
// The first signal requests drain; the second ends the wait with a failure.
// Returning on the second signal allows main to flush logs and force process exit.
func SuperviseSignals(parent context.Context, signals <-chan os.Signal, timeout time.Duration, runners map[string]Runner) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, timeout, runners) }()
	stopping := false
	for {
		select {
		case err := <-done:
			return err
		case _, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			if stopping {
				return ErrForcedShutdown
			}
			stopping = true
			cancel()
		}
	}
}
