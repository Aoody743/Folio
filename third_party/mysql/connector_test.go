package mysql

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestConnectorReturnsTimeout(t *testing.T) {
	config := NewConfig()
	config.Net = "tcp"
	config.Addr = "127.0.0.1:1234"
	config.Timeout = 10 * time.Millisecond
	var deadline time.Time
	called := false
	config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		called = true
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			return nil, errors.New("connector did not propagate its dial timeout")
		}
		if network != "tcp" || address != config.Addr {
			return nil, errors.New("connector changed its dial target")
		}
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}, Err: ctx.Err()}
	}
	connector := newConnector(config)
	started := time.Now()

	_, err := connector.Connect(context.Background())
	if err == nil {
		t.Fatal("error expected")
	}

	if nerr, ok := err.(*net.OpError); ok {
		if !called || !nerr.Timeout() || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected typed propagated timeout, got %v", err)
		}
		if delta := deadline.Sub(started); delta < config.Timeout || delta > config.Timeout+5*time.Millisecond {
			t.Fatalf("10ms dial deadline was not propagated: %v", delta)
		}
	} else {
		t.Fatalf("expected %T, got %T", nerr, err)
	}
}
