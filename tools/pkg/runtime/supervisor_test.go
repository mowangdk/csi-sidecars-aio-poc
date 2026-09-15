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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestSuperviseNormalDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	var stopped sync.WaitGroup
	stopped.Add(2)
	runners := map[string]Runner{}
	for _, name := range []string{"attacher", "snapshotter"} {
		runners[name] = func(ctx context.Context) error {
			defer stopped.Done()
			started <- name
			<-ctx.Done()
			<-release
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, time.Second, runners) }()
	seen := map[string]bool{<-started: true, <-started: true}
	if !seen["attacher"] || !seen["snapshotter"] {
		t.Fatalf("incorrect runner dispatch: %v", seen)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before drain: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopped.Wait()
}

func TestSuperviseFailures(t *testing.T) {
	failure := errors.New("driver disconnected")
	for _, tc := range []struct {
		name    string
		failure error
		want    error
	}{
		{"early return", nil, ErrUnexpectedExit},
		{"controller error", failure, failure},
		{"unsolicited cancellation", context.Canceled, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerStarted := make(chan struct{})
			peerStopped := make(chan struct{})
			err := Supervise(context.Background(), time.Second, map[string]Runner{
				"attacher": func(context.Context) error { <-peerStarted; return tc.failure },
				"resizer": func(ctx context.Context) error {
					close(peerStarted)
					<-ctx.Done()
					close(peerStopped)
					return nil
				},
			})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "attacher:") {
				t.Fatalf("lost failure attribution: %v", err)
			}
			select {
			case <-peerStopped:
			default:
				t.Fatal("peer was not drained")
			}
		})
	}
}

func TestSupervisePreservesFailureDuringCancellation(t *testing.T) {
	primary := errors.New("initial failure")
	secondary := errors.New("independent failure during drain")
	err := Supervise(context.Background(), time.Second, map[string]Runner{
		"attacher": func(context.Context) error { return primary },
		"resizer": func(ctx context.Context) error {
			<-ctx.Done()
			return errors.Join(ctx.Err(), secondary)
		},
	})
	if !errors.Is(err, primary) || !errors.Is(err, secondary) || !strings.Contains(err.Error(), "resizer:") {
		t.Fatalf("lost errors during cancellation: %v", err)
	}
}

func TestShutdownDeadlineStartsAtDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- Supervise(ctx, 25*time.Second, map[string]Runner{
				"snapshotter": func(context.Context) error { <-release; return nil },
			})
		}()
		synctest.Wait()
		// Advance virtual time beyond the deadline while the process is healthy.
		time.Sleep(time.Minute)
		select {
		case err := <-done:
			t.Fatalf("deadline started before cancellation: %v", err)
		default:
		}
		cancel()
		start := time.Now()
		err := <-done
		close(release)
		if !errors.Is(err, ErrShutdownTimeout) || !strings.Contains(err.Error(), "snapshotter") {
			t.Fatalf("unexpected timeout: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 25*time.Second {
			t.Fatalf("drain took %s, want 25s", elapsed)
		}
	})
}

func TestFailureSurvivesShutdownTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		failure := errors.New("failed initialization")
		err := Supervise(context.Background(), time.Second, map[string]Runner{
			"attacher":    func(context.Context) error { return failure },
			"provisioner": func(context.Context) error { <-release; return nil },
		})
		close(release)
		if !errors.Is(err, failure) || !errors.Is(err, ErrShutdownTimeout) {
			t.Fatalf("timeout overwrote original failure: %v", err)
		}
	})
}

func TestSignalDrainAndForce(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			signals := make(chan os.Signal, 2)
			started := make(chan struct{})
			draining := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- SuperviseSignals(context.Background(), signals, time.Second, map[string]Runner{
					"attacher": func(ctx context.Context) error {
						close(started)
						<-ctx.Done()
						close(draining)
						<-release
						return nil
					},
				})
			}()
			<-started
			signals <- signal
			<-draining
			signals <- signal
			if err := <-done; !errors.Is(err, ErrForcedShutdown) {
				t.Fatalf("second signal did not force exit: %v", err)
			}
			close(release)
		})
	}
}

func TestAsyncFailureCancelsPeersBeforeDrainCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		peerStopped := make(chan struct{})
		failure := errors.New("leadership lost")
		err := Supervise(context.Background(), time.Second, map[string]Runner{
			"attacher": func(ctx context.Context) error {
				ReportFailure(ctx, failure)
				ReportFailure(ctx, failure)
				<-release
				return nil
			},
			"resizer": func(ctx context.Context) error {
				<-ctx.Done()
				close(peerStopped)
				return nil
			},
		})
		close(release)
		if !errors.Is(err, failure) || !errors.Is(err, ErrShutdownTimeout) {
			t.Fatalf("async failure did not trigger bounded drain: %v", err)
		}
		select {
		case <-peerStopped:
		default:
			t.Fatal("peer was not canceled before failed runner drained")
		}
	})
}

func TestSignalProcessHelper(t *testing.T) {
	mode := os.Getenv("CSI_AIO_SIGNAL_HELPER")
	if mode == "" {
		return
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	timeout := 5 * time.Second
	if mode == "timeout" {
		timeout = 50 * time.Millisecond
	}
	err := SuperviseSignals(context.Background(), signals, timeout, map[string]Runner{
		"attacher": func(ctx context.Context) error {
			fmt.Println("ready")
			<-ctx.Done()
			fmt.Println("draining")
			if mode == "force" || mode == "timeout" {
				select {}
			}
			if mode == "panic" {
				panic("controller panic")
			}
			return nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRealProcessSignals(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		signal os.Signal
		want   string
	}{
		{"normal", syscall.SIGTERM, ""},
		{"normal", syscall.SIGINT, ""},
		{"force", syscall.SIGTERM, ErrForcedShutdown.Error()},
		{"timeout", syscall.SIGINT, ErrShutdownTimeout.Error()},
		{"panic", syscall.SIGTERM, "controller panic"},
	} {
		t.Run(tc.mode+"/"+tc.signal.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalProcessHelper$")
			cmd.Env = append(os.Environ(), "CSI_AIO_SIGNAL_HELPER="+tc.mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			for _, want := range []string{"ready\n", "draining\n"} {
				line, err := reader.ReadString('\n')
				if err != nil || line != want {
					cancel()
					_ = cmd.Wait()
					t.Fatalf("child output=%q err=%v stderr=%s", line, err, &stderr)
				}
				if want == "ready\n" || tc.mode == "force" {
					if err := cmd.Process.Signal(tc.signal); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = cmd.Wait()
			if ctx.Err() != nil || (err == nil) != (tc.want == "") || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("child exit=%v context=%v stderr=%s", err, ctx.Err(), &stderr)
			}
		})
	}
}

func TestInvalidSupervisionNeverStartsRunners(t *testing.T) {
	never := func(context.Context) error { t.Error("invalid invocation started runner"); return nil }
	for _, runners := range []map[string]Runner{nil, {"": never}, {"attacher": nil}} {
		if err := Supervise(context.Background(), time.Second, runners); err == nil {
			t.Fatal("invalid runners accepted")
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if err := Supervise(context.Background(), timeout, map[string]Runner{"attacher": never}); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Supervise(ctx, time.Second, map[string]Runner{"attacher": never}); err != nil {
		t.Fatal(err)
	}
}
