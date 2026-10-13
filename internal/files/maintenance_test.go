package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

type maintenanceSink struct {
	started, unblock chan struct{}
	once             sync.Once
}

func (s *maintenanceSink) Write(data []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.unblock
	return len(data), nil
}
func (s *maintenanceSink) Close() error { return nil }

func TestRootMaintenanceDrainsStreamsBlocksNewRequestsAndRepins(t *testing.T) {
	service, path := fixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(path, "value"), []byte("original"), 0600)
	upload, err := service.UploadBegin(ctx, "human", Input{Root: "workspace", Path: "pending", Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Service: service}
	sink := &maintenanceSink{started: make(chan struct{}), unblock: make(chan struct{})}
	streamDone := make(chan error, 1)
	go func() {
		_, err := executor.Stream(ctx, agent.Request{ActorID: "human", Operation: "fs_download_stream", Input: encode(Input{Root: "workspace", Path: "value"})}, agent.IO{Stdout: sink})
		streamDone <- err
	}()
	<-sink.started
	callback := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- service.RootTransaction(ctx, "workspace", func() error {
			close(callback)
			if err := os.Rename(path, path+"-old"); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0750); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(path, "value"), []byte("replacement"), 0600); err != nil {
				return err
			}
			if err := service.DeleteRoot("workspace"); err != nil {
				return err
			}
			return service.AddRoot(RootSpec{ID: "workspace", Path: path})
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.RLock()
		paused := service.maintenance["workspace"] != nil
		service.mu.RUnlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not enter pause")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-callback:
		t.Fatal("maintenance raced active download")
	default:
	}
	requestCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = executor.Run(requestCtx, agent.Request{ActorID: "human", Operation: "fs_read", Input: encode(Input{Root: "workspace", Path: "value"})})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("new read bypassed maintenance pause", err)
	}
	close(sink.unblock)
	if err := <-streamDone; err != nil {
		t.Fatal(err)
	}
	if err := <-maintenanceDone; err != nil {
		t.Fatal(err)
	}
	result, err := executor.Run(ctx, agent.Request{ActorID: "human", Operation: "fs_read", Input: encode(Input{Root: "workspace", Path: "value"})})
	if err != nil || !containsJSONText(result.Data, "replacement") {
		t.Fatal("new request retained obsolete root descriptor", err)
	}
	if _, err := service.UploadCommit(ctx, "human", Input{Root: "workspace", ID: upload.ID}); !IsCode(err, "conflict") {
		t.Fatal("old pending upload wrote into replacement root", err)
	}
}

func containsJSONText(data []byte, text string) bool {
	var out Text
	return json.Unmarshal(data, &out) == nil && out.Content == text
}

func TestRootMaintenanceFailureRestoresPriorPinnedDescriptor(t *testing.T) {
	service, path := fixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(path, "value"), []byte("original"), 0600)
	err := service.RootTransaction(ctx, "workspace", func() error {
		service.DeleteRoot("workspace")
		replacement := t.TempDir()
		os.WriteFile(filepath.Join(replacement, "value"), []byte("wrong"), 0600)
		if err := service.AddRoot(RootSpec{ID: "workspace", Path: replacement}); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	text, err := service.ReadText(ctx, Input{Root: "workspace", Path: "value"})
	if err != nil || text.Content != "original" {
		t.Fatal("failed root maintenance lost original descriptor", err)
	}
}
