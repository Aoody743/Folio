package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPLifecycleCancelsAndDrainsBeforeClosingSharedState(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	life := newHTTPLifecycle(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		<-release
		close(finished)
	}))
	go life.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/events", nil))
	<-started
	closed := make(chan struct{})
	go func() { life.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("closed shared state with active handler")
	case <-time.After(20 * time.Millisecond):
	}
	r := httptest.NewRecorder()
	life.ServeHTTP(r, httptest.NewRequest("GET", "/api/v1/system", nil))
	if r.Code != 503 {
		t.Fatal("work admitted during shutdown", r.Code)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	select {
	case <-finished:
	default:
		t.Fatal("active handler not finished")
	}
}
