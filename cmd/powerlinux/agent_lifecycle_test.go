package main

import (
	"context"
	"errors"
	"github.com/Aoody743/PowerLinux/internal/agent"
	"sync"
	"testing"
	"time"
)

type cancellationExecutor struct {
	started, release, finished chan struct{}
	once                       sync.Once
}

func (e *cancellationExecutor) Validate(agent.Request) error { return nil }
func (e *cancellationExecutor) Run(ctx context.Context, _ agent.Request) (agent.Result, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	<-e.release
	close(e.finished)
	return agent.Result{}, ctx.Err()
}
func TestAgentLifecycleCancelsAndDrainsBeforeClosingState(t *testing.T) {
	executor := &cancellationExecutor{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	life := newAgentLifecycle(context.Background(), executor)
	go life.Run(context.Background(), agent.Request{Operation: "fixture"})
	<-executor.started
	closed := make(chan struct{})
	go func() { life.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("closed state before in-flight transaction drained")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := life.Run(context.Background(), agent.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatal("new work admitted while closing", err)
	}
	close(executor.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	select {
	case <-executor.finished:
	default:
		t.Fatal("handler not finished")
	}
}
