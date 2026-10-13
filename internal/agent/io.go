package agent

import (
	"context"
	"io"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(buffer)
}

type StreamExecutor interface {
	Stream(context.Context, Request, IO) (Result, error)
}
