package plugins

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFPMHealthRequiresProtocolBodyAndSuccessfulCompletion(t *testing.T) {
	for _, test := range []struct {
		name, body string
		end        []byte
		success    bool
	}{
		{"actual-ping", "Content-Type: text/plain\r\n\r\n" + phpHealthResponse, make([]byte, 8), true},
		{"another-application", "Content-Type: text/plain\r\n\r\nnot our ping", make([]byte, 8), false},
		{"failed-completion", "Content-Type: text/plain\r\n\r\n" + phpHealthResponse, []byte{0, 0, 0, 1, 0, 0, 0, 0}, false},
		{"socket-only", "", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "fpm.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			request := make(chan string, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(time.Second))
				buffer := make([]byte, 4096)
				n, err := connection.Read(buffer)
				if err != nil {
					return
				}
				request <- string(buffer[:n])
				if test.end == nil {
					return
				}
				_, _ = connection.Write(fastCGIRecord(6, []byte(test.body)))
				_, _ = connection.Write(fastCGIRecord(3, test.end))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = probePHPFastCGI(ctx, socket)
			if (err == nil) != test.success {
				t.Fatal("incorrect protocol health result", err)
			}
			select {
			case raw := <-request:
				if !strings.Contains(raw, phpHealthPath) || !strings.Contains(raw, "SCRIPT_NAME") {
					t.Fatal("health did not request only controlled ping")
				}
			case <-ctx.Done():
				t.Fatal("probe did not send actual FastCGI request")
			}
		})
	}
}

func TestFPMHealthCancellationClosesUnresponsiveSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "fpm.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, connection)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- probePHPFastCGI(ctx, socket) }()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled unresponsive FPM marked healthy")
		}
	case <-time.After(time.Second):
		t.Fatal("FPM cancellation did not close socket")
	}
}
