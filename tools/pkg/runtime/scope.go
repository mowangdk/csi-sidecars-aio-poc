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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Scope owns an embedded entrypoint's asynchronous services. Register services
// before calling Close; Close cancels and joins them without recovering panics.
type Scope struct {
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	mu       sync.Mutex
	failures []error
	cleanup  []func()
}

func NewScope(parent context.Context) *Scope {
	ctx, cancel := context.WithCancel(parent)
	return &Scope{ctx: ctx, cancel: cancel}
}

func (s *Scope) Context() context.Context { return s.ctx }

// RunWorkerController uses the generated AIO method without extending upstream
// controller interfaces or changing standalone implementations and mocks.
func RunWorkerController(controller any, workers int, ctx context.Context, wg *sync.WaitGroup) error {
	runner, ok := controller.(interface {
		RunAIO(int, context.Context, *sync.WaitGroup)
	})
	if !ok {
		return fmt.Errorf("controller %T has no checked AIO worker adaptation", controller)
	}
	runner.RunAIO(workers, ctx, wg)
	return nil
}

func (s *Scope) Fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failures = append(s.failures, err)
	s.mu.Unlock()
	ReportFailure(s.ctx, err)
	s.cancel()
}

func (s *Scope) Go(name string, run Runner) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		err := run(s.ctx)
		if err == nil && s.ctx.Err() == nil {
			err = ErrUnexpectedExit
		} else if s.ctx.Err() != nil && err == s.ctx.Err() {
			err = nil
		}
		if err != nil {
			s.Fail(fmt.Errorf("%s: %w", name, err))
		}
	}()
}

// ConnectionLost is compatible with csi-lib-utils connection.OnConnectionLoss.
// Normal connection closure during shutdown must not become a fresh failure.
func (s *Scope) ConnectionLost(context.Context) bool {
	if s.ctx.Err() == nil {
		s.Fail(errors.New("lost connection to CSI driver"))
	}
	return false
}

// Defer registers resource cleanup after cancellation and service drain.
// Like Go, cleanup is last-in-first-out. Register only before Close.
func (s *Scope) Defer(cleanup func()) { s.cleanup = append(s.cleanup, cleanup) }

func (s *Scope) Close(err error) error {
	if s.ctx.Err() != nil && err == s.ctx.Err() {
		err = nil
	}
	if err != nil {
		ReportFailure(s.ctx, err)
	}
	s.cancel()
	s.workers.Wait()
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(append([]error{err}, s.failures...)...)
}

// ServeHTTP binds synchronously and reports asynchronous serving failures to
// the runner supervisor. Requests inherit the runner context. Shutdown is
// bounded both by the supplied timeout and by the supervisor's process deadline.
func (s *Scope) ServeHTTP(address string, handler http.Handler, shutdownTimeout time.Duration) error {
	if shutdownTimeout <= 0 {
		return fmt.Errorf("HTTP shutdown timeout must be positive")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("bind HTTP endpoint %q: %w", address, err)
	}
	s.serveHTTP(listener, handler, shutdownTimeout)
	return nil
}

func (s *Scope) serveHTTP(listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
	}
	s.Go("http", func(ctx context.Context) error {
		defer server.Close()
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		select {
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			err := server.Shutdown(shutdownCtx)
			serveErr := <-done
			if errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = nil
			}
			return errors.Join(err, serveErr)
		}
	})
}
