package plugins

import (
	"bytes"
	"errors"
	"net"
	"strconv"
)

var errCacheResponseLimit = errors.New("cache protocol response exceeds bounds")

// Run after the standard verified TLS dialer and before the driver's RESP
// decoder, so hostile declared lengths cannot allocate an unbounded buffer.
// Each action uses a fresh client; these limits cover its entire connection.
type boundedRedisConn struct {
	net.Conn
	line      []byte
	remaining int64
	stack     []int64
	bytes     int64
	headers   int
}

func (c *boundedRedisConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	if n == 0 {
		return n, err
	}
	if e := c.inspect(data[:n]); e != nil {
		c.Conn.Close()
		return 0, e
	}
	return n, err
}
func (c *boundedRedisConn) inspect(data []byte) error {
	c.bytes += int64(len(data))
	if c.bytes > 2<<20 {
		return errCacheResponseLimit
	}
	for len(data) > 0 {
		if c.remaining > 0 {
			take := min(int64(len(data)), c.remaining)
			c.remaining -= take
			data = data[take:]
			if c.remaining == 0 {
				c.pop()
			}
			continue
		}
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			c.line = append(c.line, data...)
			if len(c.line) > 4096 {
				return errCacheResponseLimit
			}
			return nil
		}
		c.line = append(c.line, data[:index+1]...)
		data = data[index+1:]
		if len(c.line) > 4096 || len(c.line) < 3 || c.line[len(c.line)-2] != '\r' {
			return errCacheResponseLimit
		}
		line := c.line[:len(c.line)-2]
		c.headers++
		if c.headers > 16384 {
			return errCacheResponseLimit
		}
		if len(c.stack) > 0 {
			c.stack[len(c.stack)-1]--
		}
		switch line[0] {
		case '+', '-', ':':
			c.pop()
		case '$':
			size, err := strconv.ParseInt(string(line[1:]), 10, 64)
			if err != nil || size < -1 || size > 1<<20 {
				return errCacheResponseLimit
			}
			if size >= 0 {
				c.remaining = size + 2
			} else {
				c.pop()
			}
		case '*':
			size, err := strconv.ParseInt(string(line[1:]), 10, 64)
			if err != nil || size < -1 || size > 8192 {
				return errCacheResponseLimit
			}
			if size > 0 {
				c.stack = append(c.stack, size)
				if len(c.stack) > 32 {
					return errCacheResponseLimit
				}
			} else {
				c.pop()
			}
		default:
			return errCacheResponseLimit
		}
		c.line = c.line[:0]
	}
	return nil
}
func (c *boundedRedisConn) pop() {
	for len(c.stack) > 0 && c.stack[len(c.stack)-1] == 0 {
		c.stack = c.stack[:len(c.stack)-1]
	}
}
