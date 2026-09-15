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

package csistartup

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCancellationClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	independent := errors.New("independent driver failure")
	for _, tc := range []struct {
		name     string
		cause    error
		canceled bool
	}{
		{"context", context.Canceled, true},
		{"wrapped context", fmt.Errorf("probe: %w", context.Canceled), true},
		{"grpc", status.Error(codes.Canceled, "request canceled"), true},
		{"wrapped grpc", fmt.Errorf("connect: %w", status.Error(codes.Canceled, "canceled")), true},
		{"own RPC deadline", status.Error(codes.DeadlineExceeded, "RPC timeout"), false},
		{"independent", independent, false},
		{"joined independent", errors.Join(context.Canceled, independent), false},
		{"joined cancellations", errors.Join(context.Canceled, status.Error(codes.Canceled, "canceled")), true},
		{"configuration", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Error(ctx, "startup", tc.cause)
			if (got == context.Canceled) != tc.canceled {
				t.Fatalf("result=%v", got)
			}
			if !tc.canceled && tc.cause != nil && !errors.Is(got, tc.cause) {
				t.Fatalf("lost cause: %v", got)
			}
		})
	}
	if got := Error(context.Background(), "startup", context.Canceled); got == context.Canceled {
		t.Fatal("unsolicited cancellation suppressed")
	}
}

func TestProbeRetriesAndPreservesConcurrentFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		err := probeForever(context.Background(), 7*time.Second, func(ctx context.Context) (bool, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Sub(time.Now()) != 7*time.Second {
				t.Error("missing per-probe deadline")
			}
			if calls == 1 {
				return false, status.Error(codes.DeadlineExceeded, "not ready")
			}
			return calls == 3, nil
		})
		if err != nil || calls != 3 || time.Since(start) != 2*time.Second {
			t.Fatalf("calls=%d elapsed=%s error=%v", calls, time.Since(start), err)
		}
	})
	for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
		ctx, cancel := context.WithCancel(context.Background())
		independent := errors.New("driver failed independently")
		err := probeForever(ctx, time.Second, func(context.Context) (bool, error) {
			cancel()
			return false, errors.Join(status.Error(code, "request ended"), independent)
		})
		if !errors.Is(err, independent) {
			t.Fatalf("lost failure racing cancellation: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := probeForever(ctx, time.Second, func(context.Context) (bool, error) { cancel(); return false, status.Error(codes.Canceled, "canceled") })
	if err != context.Canceled {
		t.Fatalf("requested cancellation became failure: %v", err)
	}
}

func TestDriverNameDeadlineAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := DriverName(ctx, func(request context.Context) (string, error) {
				deadline, ok := request.Deadline()
				if !ok || deadline.Sub(time.Now()) != 7*time.Second {
					t.Error("wrong request deadline")
				}
				close(started)
				<-request.Done()
				return "", request.Err()
			}, 7*time.Second)
			done <- err
		}()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
