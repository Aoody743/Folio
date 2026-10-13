package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerStreamsFullArtifactsWithoutLoggingContents(t *testing.T) {
	runner := helperRunner(t, 20*time.Second)
	file, err := os.Create(filepath.Join(t.TempDir(), "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content := strings.Repeat("private-database-fixture\n", 100000)
	result, err := runner.Stream(context.Background(), helperRequest("copy"), IO{Stdin: io.NopCloser(strings.NewReader(content)), Stdout: file})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "" || result.Truncated || result.StdoutBytes != int64(len(content)) {
		t.Fatalf("artifact was exposed/truncated: %+v", result)
	}
	file.Seek(0, 0)
	data, err := io.ReadAll(file)
	if err != nil || string(data) != content {
		t.Fatal("artifact stream lost bytes", err)
	}
}
func TestRunnerCancelsBlockedStreamSink(t *testing.T) {
	runner := helperRunner(t, 20*time.Second)
	reader, writer := io.Pipe()
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := runner.Stream(ctx, helperRequest("flood"), IO{Stdout: writer})
	if !errors.Is(err, context.DeadlineExceeded) || !result.TimedOut {
		t.Fatalf("blocked stream did not honor cancellation: %+v %v", result, err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("blocked artifact sink delayed cancellation")
	}
}

type failingArtifactReader struct{ used bool }

func (r *failingArtifactReader) Read(data []byte) (int, error) {
	if !r.used {
		r.used = true
		return copy(data, "partial-artifact"), nil
	}
	return 0, errors.New("fixture artifact read failed")
}
func (r *failingArtifactReader) Close() error { return nil }

func TestRunnerRejectsFailedInputEvenWhenProgramExitsSuccessfully(t *testing.T) {
	runner := helperRunner(t, 20*time.Second)
	file, err := os.Create(filepath.Join(t.TempDir(), "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := runner.Stream(context.Background(), helperRequest("copy"), IO{Stdin: &failingArtifactReader{}, Stdout: file})
	if err == nil || !strings.Contains(err.Error(), "input stream failed") || result.ExitCode != 0 {
		t.Fatalf("truncated input incorrectly marked successful: %+v %v", result, err)
	}
}
