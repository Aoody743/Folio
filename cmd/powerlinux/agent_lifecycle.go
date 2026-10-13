package main

import (
	"context"
	"github.com/Aoody743/PowerLinux/internal/agent"
	"sync"
)

// The socket closes before domain state. Cancelling and draining active work
// prevents a cancelled ACME/file transaction from racing closed descriptors.
type agentLifecycle struct {
	executor agent.Executor
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	wg       sync.WaitGroup
	closing  bool
}

func newAgentLifecycle(ctx context.Context, executor agent.Executor) *agentLifecycle {
	ctx, cancel := context.WithCancel(ctx)
	return &agentLifecycle{executor: executor, ctx: ctx, cancel: cancel}
}
func (l *agentLifecycle) Validate(r agent.Request) error {
	_, done, err := l.begin(context.Background())
	if err != nil {
		return err
	}
	defer done()
	return l.executor.Validate(r)
}
func (l *agentLifecycle) begin(ctx context.Context) (context.Context, func(), error) {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return nil, nil, context.Canceled
	}
	l.wg.Add(1)
	l.mu.Unlock()
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel()
	}
	return child, func() { stop(); cancel(); l.wg.Done() }, nil
}
func (l *agentLifecycle) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	child, done, err := l.begin(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	defer done()
	return l.executor.Run(child, r)
}
func (l *agentLifecycle) Stream(ctx context.Context, r agent.Request, streams agent.IO) (agent.Result, error) {
	child, done, err := l.begin(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	defer done()
	streamer, ok := l.executor.(agent.StreamExecutor)
	if !ok {
		return agent.Result{}, agent.ErrUnauthorized
	}
	return streamer.Stream(child, r, streams)
}
func (l *agentLifecycle) Close() {
	l.mu.Lock()
	l.closing = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
}
