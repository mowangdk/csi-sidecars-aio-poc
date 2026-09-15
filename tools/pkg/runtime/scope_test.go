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
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScopeJoinsServicesAndPreservesErrors(t *testing.T) {
	scope := NewScope(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	failure := errors.New("asynchronous failure")
	scope.Go("service", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return failure
	})
	<-started
	original := errors.New("initialization failed")
	err := scope.Close(original)
	if !errors.Is(err, original) || !errors.Is(err, failure) || !strings.Contains(err.Error(), "service:") {
		t.Fatalf("lost errors: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("service not joined")
	}
}

func TestScopeCleanupFollowsServiceDrainInReverseOrder(t *testing.T) {
	scope := NewScope(context.Background())
	var order []string
	scope.Defer(func() { order = append(order, "first resource") })
	scope.Defer(func() { order = append(order, "second resource") })
	scope.Go("worker", func(ctx context.Context) error {
		<-ctx.Done()
		order = append(order, "worker stopped")
		return nil
	})
	if err := scope.Close(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "worker stopped,second resource,first resource" {
		t.Fatalf("cleanup order=%v", order)
	}
}

type aioTestWorker struct {
	run func(int, context.Context, *sync.WaitGroup)
}

func (w aioTestWorker) RunAIO(n int, ctx context.Context, group *sync.WaitGroup) {
	w.run(n, ctx, group)
}

func TestWorkerBridgeRequiresAdaptationAndPassesLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var group sync.WaitGroup
	if err := RunWorkerController(struct{}{}, 3, ctx, &group); err == nil {
		t.Fatal("accepted unchecked controller")
	}
	stopped := make(chan struct{})
	worker := aioTestWorker{run: func(n int, got context.Context, wg *sync.WaitGroup) {
		if n != 3 || got != ctx || wg != &group {
			t.Fatal("lost worker configuration")
		}
		wg.Go(func() { <-got.Done(); close(stopped) })
	}}
	if err := RunWorkerController(worker, 3, ctx, &group); err != nil {
		t.Fatal(err)
	}
	cancel()
	group.Wait()
	select {
	case <-stopped:
	default:
		t.Fatal("worker not joined")
	}
}

func TestConnectionLossIsFailureExceptDuringShutdown(t *testing.T) {
	scope := NewScope(context.Background())
	if scope.ConnectionLost(context.Background()) {
		t.Fatal("unexpected reconnect request")
	}
	<-scope.Context().Done()
	if err := scope.Close(nil); err == nil {
		t.Fatal("connection loss was discarded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	scope = NewScope(ctx)
	cancel()
	scope.ConnectionLost(ctx)
	if err := scope.Close(ctx.Err()); err != nil {
		t.Fatalf("shutdown became failure: %v", err)
	}
}

func TestHTTPBindFailureIsSynchronous(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	scope := NewScope(context.Background())
	defer scope.Close(nil)
	if err := scope.ServeHTTP(listener.Addr().String(), http.NewServeMux(), time.Second); err == nil {
		t.Fatal("occupied address was accepted")
	}
}

func TestHTTPRequestsAreCanceledAndDrained(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := NewScope(ctx)
	started, canceled, drain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
		<-drain
		_, _ = io.WriteString(w, "drained")
	})
	scope.serveHTTP(listener, handler, 2*time.Second)
	clientDone := make(chan error, 1)
	go func() {
		client := http.Client{Timeout: 5 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				err = readErr
			} else if string(body) != "drained" {
				err = errors.New("response was not drained")
			}
		}
		clientDone <- err
	}()
	<-started
	cancel()
	<-canceled
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(nil) }()
	select {
	case err := <-closed:
		t.Fatalf("returned before request drain: %v", err)
	default:
	}
	close(drain)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		connection.Close()
		t.Fatal("listener remained open")
	}
}

func TestHTTPServerFailureReachesSupervisor(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	go func() { <-started; listener.Close() }()
	err = Supervise(context.Background(), time.Second, map[string]Runner{
		"attacher": func(ctx context.Context) error {
			scope := NewScope(ctx)
			scope.serveHTTP(listener, http.NewServeMux(), time.Second)
			close(started)
			<-scope.Context().Done()
			return scope.Close(nil)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "attacher:") || !strings.Contains(err.Error(), "http:") {
		t.Fatalf("server failure lost attribution: %v", err)
	}
}
