package main

import (
	"context"
	"net/http"
	"sync"
)

type httpLifecycle struct {
	handler http.Handler
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	wg      sync.WaitGroup
	closing bool
}

func newHTTPLifecycle(ctx context.Context, handler http.Handler) *httpLifecycle {
	ctx, cancel := context.WithCancel(ctx)
	return &httpLifecycle{handler: handler, ctx: ctx, cancel: cancel}
}
func (l *httpLifecycle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		http.Error(w, "Service is restarting", http.StatusServiceUnavailable)
		return
	}
	l.wg.Add(1)
	l.mu.Unlock()
	defer l.wg.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	defer cancel()
	if l.ctx.Err() != nil {
		cancel()
	}
	l.handler.ServeHTTP(w, r.WithContext(ctx))
}
func (l *httpLifecycle) Close() {
	l.mu.Lock()
	l.closing = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
}
