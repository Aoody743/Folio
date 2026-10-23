package serverbrowser

// Wisp v1 wire protocol: https://github.com/MercuryWorkshop/wisp-protocol
// Protocol specification © Mercury Workshop, CC BY 4.0. This implementation
// deliberately grants server-network access only to authenticated browser users.
import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/httpapi"
	"github.com/coder/websocket"
)

const wispWindow = 8
const wispMaxStreams = 32

type wispConnection struct {
	provider     *Provider
	actor        core.Actor
	ctx          context.Context
	cancel       context.CancelFunc
	ws           *websocket.Conn
	mu           sync.Mutex
	writeMu      sync.Mutex
	streams      map[uint32]*wispStream
	lastActivity time.Time
}
type wispStream struct {
	id       uint32
	conn     net.Conn
	cancel   context.CancelFunc
	packets  chan []byte
	finished chan struct{}
	mu       sync.Mutex
	closed   bool
}

func (c *wispConnection) send(kind byte, id uint32, data []byte) error {
	packet := make([]byte, 5+len(data))
	packet[0] = kind
	binary.LittleEndian.PutUint32(packet[1:5], id)
	copy(packet[5:], data)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	err := c.ws.Write(ctx, websocket.MessageBinary, packet)
	if err != nil {
		c.cancel()
	} else {
		c.mu.Lock()
		c.lastActivity = time.Now()
		c.mu.Unlock()
	}
	return err
}
func (c *wispConnection) flow(id uint32, remaining int) {
	var data [4]byte
	binary.LittleEndian.PutUint32(data[:], uint32(remaining))
	_ = c.send(3, id, data[:])
}
func (c *wispConnection) finish(s *wispStream, reason byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	if s.conn != nil {
		s.conn.Close()
	}
	s.mu.Unlock()
	_ = c.send(4, s.id, []byte{reason})
	c.mu.Lock()
	if c.streams[s.id] == s {
		delete(c.streams, s.id)
	}
	c.mu.Unlock()
	close(s.finished)
}
func (c *wispConnection) connect(id uint32, payload []byte) error {
	if id == 0 || len(payload) < 4 || len(payload) > 260 {
		return errors.New("invalid Wisp connect")
	}
	kind := payload[0]
	port := binary.LittleEndian.Uint16(payload[1:3])
	host := string(payload[3:])
	if (kind != 1 && kind != 2) || port == 0 || !utf8.ValidString(host) || strings.ContainsAny(host, "\x00/\\\r\n\t ") || host == "" {
		return errors.New("invalid Wisp destination")
	}
	c.mu.Lock()
	if prior := c.streams[id]; prior != nil {
		c.mu.Unlock()
		c.finish(prior, 0x02)
		select {
		case <-prior.finished:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
		c.mu.Lock()
	}
	if len(c.streams) >= wispMaxStreams {
		c.mu.Unlock()
		_ = c.send(4, id, []byte{0x49})
		return nil
	}
	ctx, cancel := context.WithCancel(c.ctx)
	s := &wispStream{id: id, cancel: cancel, packets: make(chan []byte, wispWindow), finished: make(chan struct{})}
	c.streams[id] = s
	c.mu.Unlock()
	go func() {
		network := "tcp"
		if kind == 2 {
			network = "udp"
		}
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(host, strconv.Itoa(int(port))))
		if err != nil {
			c.finish(s, 0x42)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conn = conn
		s.mu.Unlock()
		go func() {
			buf := make([]byte, 65536)
			for {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
				n, e := conn.Read(buf)
				if n > 0 {
					if c.send(2, id, buf[:n]) != nil {
						break
					}
				}
				if e != nil {
					break
				}
			}
			c.finish(s, 0x02)
		}()
		for {
			select {
			case <-ctx.Done():
				c.finish(s, 0x02)
				return
			case data := <-s.packets:
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				n, e := conn.Write(data)
				if e != nil || n != len(data) {
					c.finish(s, 0x03)
					return
				}
				if kind == 1 {
					c.flow(id, wispWindow-len(s.packets))
				}
			}
		}
	}()
	return nil
}
func (p *Provider) serveWisp(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	p.appGate.RLock()
	if !p.installed(r.Context(), "firefox-wasm") {
		p.appGate.RUnlock()
		reply(w, failure("browser_not_installed", "Install Firefox WASM from Applications first", 409))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	c := &wispConnection{provider: p, actor: actor, ctx: ctx, cancel: cancel, streams: map[uint32]*wispStream{}, lastActivity: time.Now()}
	p.mu.Lock()
	blocked := p.closed || len(p.wisps) >= p.options.MaxSessions
	for prior := range p.wisps {
		if prior.actor.ID == actor.ID {
			blocked = true
		}
	}
	if !blocked {
		p.wisps[c] = struct{}{}
	}
	p.mu.Unlock()
	p.appGate.RUnlock()
	if blocked {
		cancel()
		reply(w, failure("browser_limit", "Close an existing Firefox WASM window before opening another", 429))
		return
	}
	defer func() {
		cancel()
		p.mu.Lock()
		delete(p.wisps, c)
		p.mu.Unlock()
		if c.ws != nil {
			c.ws.CloseNow()
		}
		c.mu.Lock()
		all := []*wispStream{}
		for _, s := range c.streams {
			all = append(all, s)
		}
		c.mu.Unlock()
		for _, s := range all {
			c.finish(s, 0x02)
		}
	}()
	var err error
	c.ws, err = websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"folio-wisp-v1"}})
	if err != nil {
		return
	}
	c.ws.SetReadLimit(65541)
	c.flow(0, wispWindow)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		started := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fresh, e := httpapi.RevalidateActor(r)
				if e == nil {
					_, e = p.actor(ctx, fresh)
				}
				c.mu.Lock()
				idle := time.Since(c.lastActivity) > p.options.IdleTimeout
				c.mu.Unlock()
				if e != nil || idle || time.Since(started) > p.options.MaxLifetime {
					cancel()
					return
				}
			}
		}
	}()
	for {
		kind, data, e := c.ws.Read(ctx)
		if e != nil {
			return
		}
		fresh, e := httpapi.RevalidateActor(r)
		if e != nil {
			return
		}
		if _, e = p.actor(ctx, fresh); e != nil {
			return
		}
		if kind != websocket.MessageBinary || len(data) < 5 {
			return
		}
		id := binary.LittleEndian.Uint32(data[1:5])
		payload := data[5:]
		c.mu.Lock()
		c.lastActivity = time.Now()
		stream := c.streams[id]
		c.mu.Unlock()
		switch data[0] {
		case 1:
			if c.connect(id, payload) != nil {
				_ = c.send(4, id, []byte{0x41})
			}
		case 2:
			// TCP may end while DATA is already in flight. Discard late DATA
			// for that stream; it must not terminate unrelated browser tabs.
			if stream == nil {
				continue
			}
			select {
			case stream.packets <- append([]byte(nil), payload...):
			default:
				c.finish(stream, 0x49)
			}
		case 3:
			if len(payload) != 4 {
				return
			} // Wisp only flow-controls traffic towards the server.
		case 4:
			if len(payload) != 1 {
				return
			}
			if stream != nil {
				c.finish(stream, 0x02)
			}
		default:
			return
		}
	}
}
