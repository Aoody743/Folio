//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

type pipeFrame struct {
	Type   string  `json:"type"`
	Stream string  `json:"stream,omitempty"`
	Data   string  `json:"data,omitempty"`
	Result *Result `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
	Code   string  `json:"code,omitempty"`
	Status int     `json:"status,omitempty"`
}
type responseSink struct {
	ctx    context.Context
	writer http.ResponseWriter
	mu     *sync.Mutex
	stream string
}

func (s *responseSink) Write(data []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	controller := http.NewResponseController(s.writer)
	controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
	for start := 0; start < len(data); start += 16 << 10 {
		end := min(start+(16<<10), len(data))
		frame := pipeFrame{Type: "output", Stream: s.stream, Data: base64.StdEncoding.EncodeToString(data[start:end])}
		if err := json.NewEncoder(s.writer).Encode(frame); err != nil {
			return 0, err
		}
		if err := controller.Flush(); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}
func (s *responseSink) Close() error {
	return http.NewResponseController(s.writer).SetWriteDeadline(time.Now())
}

type readerCloser struct {
	io.Reader
	io.Closer
}
type requestBodyCloser struct {
	body       io.Closer
	controller *http.ResponseController
}

func (c requestBodyCloser) Close() error {
	c.controller.SetReadDeadline(time.Now())
	return c.body.Close()
}

func remoteStatus(err error) (string, int) {
	var typed interface{ AgentError() (string, int) }
	if errors.As(err, &typed) {
		return typed.AgentError()
	}
	if errors.Is(err, ErrUnauthorized) {
		return errorCode(err), 403
	}
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrUnsafePath) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrFileTooLarge) {
		return errorCode(err), 400
	}
	return errorCode(err), 500
}
func pipeHandler(executor Executor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		controller.EnableFullDuplex()
		w.Header().Set("Content-Type", "application/x-ndjson")
		body := http.MaxBytesReader(w, r.Body, (64<<30)+(64<<10))
		reader := bufio.NewReaderSize(body, 64<<10)
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > 64<<10 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(wireResponse{Error: "Invalid streaming operation metadata", Code: "invalid_input"})
			return
		}
		var input wireRequest
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(wireResponse{Error: "Invalid streaming operation metadata", Code: "invalid_input"})
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(wireResponse{Error: "Expected one streaming metadata document", Code: "invalid_input"})
			return
		}
		request := Request{ActorID: input.ActorID, JobID: input.JobID, Operation: input.Operation, Input: input.Input}
		if err := executor.Validate(request); err != nil {
			code, status := remoteStatus(err)
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(wireResponse{Error: err.Error(), Code: code})
			return
		}
		streamer, ok := executor.(StreamExecutor)
		if !ok {
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(wireResponse{Error: "This Agent operation does not support binary streaming", Code: "unsupported"})
			return
		}
		if longLivedStreamOperation(request.Operation) {
			// Keep the normal server read timeout for connection setup and
			// validation. Only a valid, explicitly bounded artifact operation may
			// extend the body deadline, and it remains capped at 30 minutes.
			if err := controller.SetReadDeadline(time.Now().Add(30 * time.Minute)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(wireResponse{Error: "Agent could not establish the bounded artifact stream deadline", Code: "stream_deadline_unavailable"})
				return
			}
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(pipeFrame{Type: "start"})
		controller.Flush()
		mu := &sync.Mutex{}
		stdout := &responseSink{ctx: r.Context(), writer: w, mu: mu, stream: "stdout"}
		stderr := &responseSink{ctx: r.Context(), writer: w, mu: mu, stream: "stderr"}
		result, err := streamer.Stream(r.Context(), request, IO{Stdin: readerCloser{Reader: reader, Closer: requestBodyCloser{body: body, controller: controller}}, Stdout: stdout, Stderr: stderr})
		frame := pipeFrame{Type: "result", Result: &result}
		if err != nil {
			frame.Error = err.Error()
			frame.Code, frame.Status = remoteStatus(err)
		}
		mu.Lock()
		controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		json.NewEncoder(w).Encode(frame)
		controller.Flush()
		mu.Unlock()
	}
}

func longLivedStreamOperation(operation string) bool {
	switch operation {
	case "backup_artifact", "database_artifact", "database_import_artifact":
		return true
	default:
		return false
	}
}

// Stream tunnels binary stdin and output over the UID-checked Unix connection.
// A bound sink receives the exact bytes without copying them into logs/results.
func (c *UnixClient) Stream(ctx context.Context, request Request, streams IO) (Result, error) {
	metadata, err := json.Marshal(wireRequest{ActorID: request.ActorID, JobID: request.JobID, Operation: request.Operation, Input: request.Input})
	if err != nil {
		return Result{}, err
	}
	if len(metadata) > 63<<10 {
		return Result{}, ErrInvalidInput
	}
	metadata = append(metadata, '\n')
	var source io.ReadCloser = io.NopCloser(bytes.NewReader(metadata))
	if streams.Stdin != nil {
		source = readerCloser{Reader: io.MultiReader(bytes.NewReader(metadata), streams.Stdin), Closer: streams.Stdin}
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/v1/pipe", source)
	if err != nil {
		return Result{}, err
	}
	r.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.client.Do(r)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		var wire wireResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&wire); err != nil {
			return Result{}, err
		}
		return wire.Result, &RemoteError{Code: wire.Code, Message: wire.Error, Status: response.StatusCode, cause: codeError(wire.Code, wire.Error)}
	}
	stop := context.AfterFunc(ctx, func() {
		if streams.Stdin != nil {
			streams.Stdin.Close()
		}
		if streams.Stdout != nil {
			streams.Stdout.Close()
		}
		if streams.Stderr != nil {
			streams.Stderr.Close()
		}
	})
	defer stop()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	captured := &boundedCapture{remaining: 1 << 20}
	for scanner.Scan() {
		var frame pipeFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return Result{}, err
		}
		switch frame.Type {
		case "start":
		case "output":
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil || len(data) > 16<<10 {
				return Result{}, errors.New("invalid Agent binary output frame")
			}
			var sink io.WriteCloser
			switch frame.Stream {
			case "stdout":
				sink = streams.Stdout
			case "stderr":
				sink = streams.Stderr
			default:
				return Result{}, errors.New("unknown Agent output stream")
			}
			if sink != nil {
				if _, err := sink.Write(data); err != nil {
					response.Body.Close()
					if ctx.Err() != nil {
						return Result{}, ctx.Err()
					}
					return Result{}, err
				}
			} else {
				captured.read(bytes.NewReader(data), frame.Stream)
			}
		case "result":
			if frame.Result == nil {
				return Result{}, errors.New("Agent stream did not return a result")
			}
			result := *frame.Result
			stdout, stderr, truncated, _ := captured.result()
			if streams.Stdout == nil {
				result.Stdout = stdout
			}
			if streams.Stderr == nil {
				result.Stderr = stderr
			}
			result.Truncated = result.Truncated || truncated
			if frame.Error != "" {
				return result, &RemoteError{Code: frame.Code, Message: frame.Error, Status: frame.Status, cause: codeError(frame.Code, frame.Error)}
			}
			return result, nil
		default:
			return Result{}, errors.New("unknown Agent protocol frame")
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}
	return Result{}, io.ErrUnexpectedEOF
}
