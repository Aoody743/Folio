package plugins

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

func memcacheConnect(ctx context.Context, p CacheConnection) (net.Conn, *bufio.Reader, error) {
	address := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if p.TLS {
		d := tls.Dialer{NetDialer: dialer, Config: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.Host}}
		conn, err = d.DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, nil, failure("connection_failed", "Memcached connection or TLS failed", 502)
	}
	if p.Endpoint != "" && canonicalCacheEndpoint(conn.RemoteAddr().String()) != canonicalCacheEndpoint(p.Endpoint) {
		conn.Close()
		return nil, nil, failure("connection_changed", "Cache peer changed; revalidate its namespace binding", 409)
	}
	deadline := time.Now().Add(10 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	conn.SetDeadline(deadline)
	return conn, bufio.NewReaderSize(conn, 4096), nil
}
func memcacheLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 || !strings.HasSuffix(string(line), "\r\n") {
		return "", failure("protocol_error", "Memcached response was invalid or timed out", 502)
	}
	return strings.TrimSuffix(string(line), "\r\n"), nil
}
func probeMemcache(ctx context.Context, p CacheConnection) error {
	_, err := probeMemcacheEndpoint(ctx, p)
	return err
}
func probeMemcacheEndpoint(ctx context.Context, p CacheConnection) (string, error) {
	conn, reader, e := memcacheConnect(ctx, p)
	if e != nil {
		return "", e
	}
	defer conn.Close()
	if _, e = io.WriteString(conn, "version\r\n"); e != nil {
		return "", failure("connection_failed", "Memcached version query failed", 502)
	}
	line, e := memcacheLine(reader)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(line, "VERSION ") {
		return "", failure("protocol_error", "Endpoint does not speak Memcached ASCII protocol", 502)
	}
	return canonicalCacheEndpoint(conn.RemoteAddr().String()), nil
}
func memcacheGet(ctx context.Context, p CacheConnection, key string) (string, bool, error) {
	conn, reader, e := memcacheConnect(ctx, p)
	if e != nil {
		return "", false, e
	}
	defer conn.Close()
	if _, e = fmt.Fprintf(conn, "get %s\r\n", key); e != nil {
		return "", false, failure("connection_failed", "Memcached read failed", 502)
	}
	line, e := memcacheLine(reader)
	if e != nil {
		return "", false, e
	}
	if line == "END" {
		return "", false, nil
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "VALUE" || fields[1] != key {
		return "", false, failure("protocol_error", "Memcached value response is invalid", 502)
	}
	size, e := strconv.Atoi(fields[3])
	if e != nil || size < 0 || size > 128<<10 {
		return "", false, failure("result_limit", "Memcached value exceeds 128 KiB", 413)
	}
	data := make([]byte, size+2)
	if _, e = io.ReadFull(reader, data); e != nil || string(data[size:]) != "\r\n" {
		return "", false, failure("protocol_error", "Memcached value was incomplete", 502)
	}
	ending, e := memcacheLine(reader)
	if e != nil || ending != "END" {
		return "", false, failure("protocol_error", "Memcached response did not terminate correctly", 502)
	}
	return string(data[:size]), true, nil
}
func memcacheSet(ctx context.Context, p CacheConnection, key, value string, ttl int64) error {
	conn, reader, e := memcacheConnect(ctx, p)
	if e != nil {
		return e
	}
	defer conn.Close()
	if _, e = fmt.Fprintf(conn, "set %s 0 %d %d\r\n%s\r\n", key, ttl, len(value), value); e != nil {
		return cacheMutationError(e)
	}
	line, e := memcacheLine(reader)
	if e != nil {
		return cacheMutationError(e)
	}
	if line != "STORED" {
		return failure("query_failed", "Memcached rejected the value", 400)
	}
	return nil
}
func memcacheDelete(ctx context.Context, p CacheConnection, key string) (bool, error) {
	conn, reader, e := memcacheConnect(ctx, p)
	if e != nil {
		return false, e
	}
	defer conn.Close()
	if _, e = fmt.Fprintf(conn, "delete %s\r\n", key); e != nil {
		return false, cacheMutationError(e)
	}
	line, e := memcacheLine(reader)
	if e != nil {
		return false, cacheMutationError(e)
	}
	if line == "NOT_FOUND" {
		return false, nil
	}
	if line != "DELETED" {
		return false, failure("query_failed", "Memcached rejected the deletion", 400)
	}
	return true, nil
}
