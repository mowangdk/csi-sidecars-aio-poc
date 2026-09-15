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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOwnConfigPreservesWrappersAndOwnsResponse(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := 0
	original := &rest.Config{WrapTransport: func(next http.RoundTripper) http.RoundTripper { wrapped++; return next }}
	config := OwnConfig(parent, original)
	if config == original {
		t.Fatal("mutated shared config")
	}
	var requestContext context.Context
	transport := config.WrapTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestContext = r.Context()
		return &http.Response{Body: io.NopCloser(strings.NewReader("stream"))}, nil
	}))
	caller, callerCancel := context.WithTimeout(context.Background(), time.Minute)
	defer callerCancel()
	request, _ := http.NewRequestWithContext(caller, "GET", "http://fixture", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	deadline, _ := caller.Deadline()
	got, _ := requestContext.Deadline()
	if wrapped != 1 || deadline != got || requestContext.Err() != nil {
		t.Fatal("wrapper, deadline, or response lifetime lost")
	}
	cancel()
	<-requestContext.Done()
	if caller.Err() != nil {
		t.Fatal("canceled caller-owned context")
	}
	originalTransport := original.WrapTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() != nil {
			t.Fatal("original config inherited AIO cancellation")
		}
		return nil, nil
	}))
	_, _ = originalTransport.RoundTrip(request)
}

func TestOwnConfigCancellationDuringRequest(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	transport := OwnConfig(parent, &rest.Config{}).WrapTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	done := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest("GET", "http://fixture", nil)
		_, err := transport.RoundTrip(request)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
