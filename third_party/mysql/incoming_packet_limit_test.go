package mysql

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// Feed real packet bytes in exact chunks. A rejected header must never cause
// a second Read into a body-sized allocation, even when the body is withheld.
type receiveLimitConn struct {
	net.Conn
	chunks    [][]byte
	reads     int
	readBytes int
	closed    bool
}

func (c *receiveLimitConn) Read(p []byte) (int, error) {
	c.reads++
	if c.Conn != nil {
		n, e := c.Conn.Read(p)
		c.readBytes += n
		return n, e
	}
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	if len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}
func (c *receiveLimitConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *receiveLimitConn) Close() error {
	c.closed = true
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}
func (c *receiveLimitConn) SetDeadline(time.Time) error      { return nil }
func (c *receiveLimitConn) SetReadDeadline(time.Time) error  { return nil }
func (c *receiveLimitConn) SetWriteDeadline(time.Time) error { return nil }
func receiveLimitMySQL(conn *receiveLimitConn, limit int) *mysqlConn {
	config := NewConfig()
	config.MaxAllowedPacket = limit
	return &mysqlConn{netConn: conn, rawConn: conn, cfg: config, buf: newBuffer(), maxAllowedPacket: limit, closech: make(chan struct{})}
}
func packetHeader(size int, sequence byte) []byte {
	return []byte{byte(size), byte(size >> 8), byte(size >> 16), sequence}
}
func TestIncomingPacketLimitRejectsHeaderWithoutBodyRead(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		socket, e := listener.Accept()
		if e != nil {
			return
		}
		defer socket.Close()
		socket.SetDeadline(time.Now().Add(time.Second))
		socket.Write(packetHeader(2<<20+1, 0))
		io.Copy(io.Discard, socket)
	}()
	socket, e := net.Dial("tcp", listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	conn := &receiveLimitConn{Conn: socket}
	mc := receiveLimitMySQL(conn, 2<<20)
	_, err := mc.readPacket()
	if !errors.Is(err, ErrPktTooLarge) || !conn.closed || conn.readBytes != 4 {
		t.Fatalf("oversized packet was not rejected before body read: err=%v reads=%d closed=%v", err, conn.reads, conn.closed)
	}
	if len(mc.buf.cachedBuf) != defaultBufSize {
		t.Fatal("oversized header grew the packet buffer")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rejected wire connection was not closed")
	}
}
func TestIncomingPacketLimitAcceptsExactBoundary(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 4096)
	conn := &receiveLimitConn{chunks: [][]byte{packetHeader(len(body), 0), body}}
	mc := receiveLimitMySQL(conn, len(body))
	got, err := mc.readPacket()
	if err != nil || !bytes.Equal(got, body) || conn.closed {
		t.Fatal("bounded packet was rejected", err)
	}
}
func TestIncomingPacketLimitIncludesSplitFragments(t *testing.T) {
	first := bytes.Repeat([]byte("a"), maxPacketSize)
	conn := &receiveLimitConn{chunks: [][]byte{packetHeader(maxPacketSize, 0), first, packetHeader(2, 1)}}
	mc := receiveLimitMySQL(conn, maxPacketSize+1)
	_, err := mc.readPacket()
	if !errors.Is(err, ErrPktTooLarge) || !conn.closed || conn.reads != 3 {
		t.Fatalf("split accumulated limit failed: %v reads=%d", err, conn.reads)
	}
	first = bytes.Repeat([]byte("b"), maxPacketSize)
	conn = &receiveLimitConn{chunks: [][]byte{packetHeader(maxPacketSize, 0), first, packetHeader(1, 1), []byte("c")}}
	mc = receiveLimitMySQL(conn, maxPacketSize+1)
	body, err := mc.readPacket()
	if err != nil || len(body) != maxPacketSize+1 || body[len(body)-1] != 'c' {
		t.Fatal("exact split boundary was rejected", err)
	}
}
func TestIncomingPacketLimitProtectsAuthenticationHandshake(t *testing.T) {
	conn := &receiveLimitConn{chunks: [][]byte{packetHeader(2<<20+1, 0)}}
	mc := receiveLimitMySQL(conn, 2<<20)
	mc.maxAllowedPacket = maxPacketSize
	_, err := mc.readPacket()
	if !errors.Is(err, ErrPktTooLarge) || conn.reads != 1 {
		t.Fatal("configured limit was ignored before authentication", err)
	}
}
