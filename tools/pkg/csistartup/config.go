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
	"io"
	"net/http"
	"sync"

	"k8s.io/client-go/rest"
)

// OwnConfig copies the config, preserving existing transport wrappers while
// binding detached discovery requests to an AIO runner. The original config is
// unchanged and must still be used by election, which renews during worker drain.
func OwnConfig(parent context.Context, config *rest.Config) *rest.Config {
	owned := rest.CopyConfig(config)
	owned.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return &ownedTransport{parent: parent, next: next}
	})
	return owned
}

type ownedTransport struct {
	parent context.Context
	next   http.RoundTripper
}

func (t *ownedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.parent, cancel)
	if t.parent.Err() != nil {
		cancel()
	}
	cleanup := func() { stop(); cancel() }
	response, err := t.next.RoundTrip(request.Clone(ctx))
	if err != nil || response == nil || response.Body == nil {
		cleanup()
		return response, err
	}
	response.Body = &ownedBody{ReadCloser: response.Body, cleanup: cleanup}
	return response, nil
}

type ownedBody struct {
	io.ReadCloser
	once    sync.Once
	cleanup func()
}

func (b *ownedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.cleanup)
	}
	return n, err
}

func (b *ownedBody) Close() error {
	defer b.once.Do(b.cleanup)
	return b.ReadCloser.Close()
}
