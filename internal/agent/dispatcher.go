package agent

import (
	"context"
	"errors"
)

// Dispatcher binds trusted module executors without importing domain modules.
// Its operation table is built by the CLI from a root-owned installation policy.
type Dispatcher struct{ executors map[string]Executor }

func NewDispatcher(operations map[string]Executor) (*Dispatcher, error) {
	result := &Dispatcher{executors: map[string]Executor{}}
	for name, executor := range operations {
		if !operationName.MatchString(name) || executor == nil {
			return nil, errors.New("invalid trusted operation binding")
		}
		result.executors[name] = executor
	}
	return result, nil
}
func (d *Dispatcher) Validate(request Request) error {
	executor, ok := d.executors[request.Operation]
	if !ok {
		return ErrUnauthorized
	}
	return executor.Validate(request)
}
func (d *Dispatcher) Run(ctx context.Context, request Request) (Result, error) {
	executor, ok := d.executors[request.Operation]
	if !ok {
		return Result{}, ErrUnauthorized
	}
	return executor.Run(ctx, request)
}
func (d *Dispatcher) Stream(ctx context.Context, request Request, streams IO) (Result, error) {
	executor, ok := d.executors[request.Operation]
	if !ok {
		return Result{}, ErrUnauthorized
	}
	streamer, ok := executor.(StreamExecutor)
	if !ok {
		return Result{}, errors.New("operation does not support streaming")
	}
	if err := executor.Validate(request); err != nil {
		return Result{}, err
	}
	return streamer.Stream(ctx, request, streams)
}

type restrictedExecutor struct {
	executor   Executor
	operations map[string]bool
}

func Restrict(executor Executor, operations []string) Executor {
	allowed := map[string]bool{}
	for _, operation := range operations {
		allowed[operation] = true
	}
	return &restrictedExecutor{executor: executor, operations: allowed}
}
func (e *restrictedExecutor) Validate(request Request) error {
	if !e.operations[request.Operation] {
		return ErrUnauthorized
	}
	return e.executor.Validate(request)
}
func (e *restrictedExecutor) Run(ctx context.Context, request Request) (Result, error) {
	if err := e.Validate(request); err != nil {
		return Result{}, err
	}
	return e.executor.Run(ctx, request)
}
func (e *restrictedExecutor) Stream(ctx context.Context, request Request, streams IO) (Result, error) {
	if err := e.Validate(request); err != nil {
		return Result{}, err
	}
	streamer, ok := e.executor.(StreamExecutor)
	if !ok {
		return Result{}, errors.New("operation does not support streaming")
	}
	return streamer.Stream(ctx, request, streams)
}
