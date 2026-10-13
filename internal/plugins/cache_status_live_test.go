package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestCacheStatusRedisActionUsesFixedReadOnlyInfoSections(t *testing.T) {
	sections := map[string]string{
		"server":      "# Server\r\nredis_version:8.0.1\r\nuptime_in_seconds:123\r\n",
		"memory":      "# Memory\r\nused_memory:9007199254740993\r\nused_memory_peak:9007199254740994\r\nmaxmemory:0\r\n",
		"persistence": "# Persistence\r\naof_enabled:1\r\naof_rewrite_in_progress:0\r\nrdb_bgsave_in_progress:0\r\nrdb_changes_since_last_save:4\r\nrdb_last_bgsave_status:ok\r\n",
		"replication": "# Replication\r\nrole:master\r\nconnected_slaves:1\r\nmaster_repl_offset:9007199254740997\r\nslave0:ip=192.0.2.44,port=6380,state=online,offset=9007199254740996,lag=7\r\nmaster_host:10.4.5.6\r\nmaster_user:private-user\r\nmaster_password:private-password\r\nconnected_clients:8\r\n",
	}
	listener, infoRequests := startFakeRedisStatusServer(t, sections)
	s, actor, _, profile := setupCacheStatusProfile(t, "redis", listener.Addr().String())
	raw, _ := json.Marshal(IDInput{ID: profile.ID})
	data, err := s.Execute(context.Background(), actor, "cache_status", raw)
	if err != nil {
		t.Fatal("read-only cache status failed", core.ErrorCode(err))
	}
	status, ok := data.(CacheStatusResult)
	if !ok || !status.Connected || status.Version != "8.0.1" || status.VersionSource != "redis_version" || status.UptimeSeconds == nil || *status.UptimeSeconds != "123" || status.Memory == nil || status.Memory.UsedBytes == nil || *status.Memory.UsedBytes != "9007199254740993" || status.Replication == nil || status.Replication.Offset == nil || *status.Replication.Offset != "9007199254740997" {
		t.Fatalf("typed cache status mismatch: %#v", data)
	}
	for _, want := range []string{"server", "memory", "persistence", "replication"} {
		select {
		case got := <-infoRequests:
			if got != want {
				t.Fatalf("unexpected Redis command section %q; want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("Redis INFO section %q was not requested", want)
		}
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	for _, private := range []string{"master_host", "master_user", "master_password", "private-user", "private-password", "192.0.2.44", "10.4.5.6", "connected_clients", "127.0.0.1"} {
		if strings.Contains(public, private) {
			t.Fatal("public cache status leaked private config or raw INFO fields", private, public)
		}
	}
	if strings.Contains(public, `"offset":9007199254740997`) || !strings.Contains(public, `"offset":"9007199254740997"`) {
		t.Fatal("large replication offset was not emitted as an exact decimal string", public)
	}
}

func TestCacheStatusValkeyReportsNativeVersionWithCompatibilityField(t *testing.T) {
	sections := map[string]string{
		"server":      "# Server\nvalkey_version:8.0.1\nredis_version:7.2.4\nuptime_in_seconds:123\n",
		"memory":      "# Memory\nused_memory:9007199254740993\n",
		"persistence": "# Persistence\n",
		"replication": "# Replication\nrole:master\n",
	}
	listener, infoRequests := startFakeRedisStatusServer(t, sections)
	s, actor, _, profile := setupCacheStatusProfile(t, "valkey", listener.Addr().String())
	raw, _ := json.Marshal(IDInput{ID: profile.ID})
	data, err := s.Execute(context.Background(), actor, "cache_status", raw)
	if err != nil {
		t.Fatal("read-only Valkey status failed", core.ErrorCode(err))
	}
	status, ok := data.(CacheStatusResult)
	if !ok || status.Version != "8.0.1" || status.VersionSource != "valkey_version" {
		t.Fatalf("Valkey native version was not reported: %#v", data)
	}
	for _, want := range []string{"server", "memory", "persistence", "replication"} {
		select {
		case got := <-infoRequests:
			if got != want {
				t.Fatalf("unexpected Valkey command section %q; want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("Valkey INFO section %q was not requested", want)
		}
	}
}

func TestCacheStatusRedisHonorsCallerDeadlineDuringDelayedInfo(t *testing.T) {
	sections := map[string]string{
		"server":      "# Server\nredis_version:8.0.1\nmaster_password:delayed-private-response\n",
		"memory":      "# Memory\nused_memory:1\n",
		"persistence": "# Persistence\n",
		"replication": "# Replication\nrole:master\n",
	}
	listener, infoRequests := startFakeRedisStatusServer(t, sections, 2*time.Second)
	s, actor, _, profile := setupCacheStatusProfile(t, "redis", listener.Addr().String())
	raw, _ := json.Marshal(IDInput{ID: profile.ID})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := s.Execute(ctx, actor, "cache_status", raw)
	elapsed := time.Since(started)
	if core.ErrorCode(err) != "query_failed" {
		t.Fatalf("delayed INFO did not fail safely on caller deadline: code=%q err=%v", core.ErrorCode(err), err)
	}
	if elapsed > time.Second {
		t.Fatalf("Redis command ignored caller deadline: elapsed=%s", elapsed)
	}
	select {
	case got := <-infoRequests:
		if got != "server" {
			t.Fatalf("unexpected Redis INFO command before deadline: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Redis INFO did not reach the delayed TCP peer")
	}
	if strings.Contains(err.Error(), "delayed-private-response") || strings.Contains(err.Error(), "master_password") {
		t.Fatalf("deadline error leaked raw INFO data: %v", err)
	}
}

func TestCacheStatusMemcachedActionReadsBoundedStats(t *testing.T) {
	listener, request := startFakeMemcacheStatusServer(t, []string{
		"STAT version 1.6.21",
		"STAT uptime 120",
		"STAT bytes 9007199254740993",
		"STAT limit_maxbytes 268435456",
		"STAT curr_items 42",
		"STAT pid 12345",
		"STAT curr_connections 9",
		"STAT address 127.0.0.1:11211",
	})
	s, actor, _, profile := setupCacheStatusProfile(t, "memcached", listener.Addr().String())
	raw, _ := json.Marshal(IDInput{ID: profile.ID})
	data, err := s.Execute(context.Background(), actor, "cache_status", raw)
	if err != nil {
		t.Fatal("Memcached status failed", core.ErrorCode(err))
	}
	status, ok := data.(CacheStatusResult)
	if !ok || !status.Connected || status.Version != "1.6.21" || status.UptimeSeconds == nil || *status.UptimeSeconds != "120" || status.Memory == nil || status.Memory.UsedBytes == nil || *status.Memory.UsedBytes != "9007199254740993" || status.Memory.LimitBytes == nil || *status.Memory.LimitBytes != "268435456" || status.Memory.Items == nil || *status.Memory.Items != "42" || status.Persistence != nil || status.Replication != nil {
		t.Fatalf("typed Memcached status mismatch: %#v", data)
	}
	select {
	case got := <-request:
		if got != "stats" {
			t.Fatalf("Memcached status sent unexpected command %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Memcached stats command was not sent")
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	for _, private := range []string{"pid", "12345", "curr_connections", "address", "127.0.0.1:11211", "host", "port"} {
		if strings.Contains(public, private) {
			t.Fatal("public Memcached status leaked an unlisted stats field", private, public)
		}
	}
}

func TestCacheStatusRejectsOversizedRealProtocolReplies(t *testing.T) {
	t.Run("redis", func(t *testing.T) {
		sections := map[string]string{
			"server": strings.Repeat("x", maxRedisInfoBytes+1),
		}
		listener, requests := startFakeRedisStatusServer(t, sections)
		s, actor, _, profile := setupCacheStatusProfile(t, "redis", listener.Addr().String())
		raw, _ := json.Marshal(IDInput{ID: profile.ID})
		_, err := s.Execute(context.Background(), actor, "cache_status", raw)
		if core.ErrorCode(err) != "result_limit" {
			t.Fatal("oversized Redis INFO reply was not rejected", core.ErrorCode(err))
		}
		if got := <-requests; got != "server" {
			t.Fatal("unexpected Redis INFO request", got)
		}
	})
	t.Run("memcached", func(t *testing.T) {
		line := "STAT unknown " + strings.Repeat("a", 3900)
		lines := make([]string, 9)
		for i := range lines {
			lines[i] = line
		}
		listener, request := startFakeMemcacheStatusServer(t, lines)
		s, actor, _, profile := setupCacheStatusProfile(t, "memcached", listener.Addr().String())
		raw, _ := json.Marshal(IDInput{ID: profile.ID})
		_, err := s.Execute(context.Background(), actor, "cache_status", raw)
		if core.ErrorCode(err) != "result_limit" {
			t.Fatal("oversized Memcached stats reply was not rejected", core.ErrorCode(err))
		}
		if got := <-request; got != "stats" {
			t.Fatal("unexpected Memcached command", got)
		}
	})
}

func TestCacheStatusDoesNotReportIncompleteMemcachedStatsAsHealthy(t *testing.T) {
	listener, request := startFakeMemcacheStatusServer(t, []string{"STAT pid 12345"})
	s, actor, _, profile := setupCacheStatusProfile(t, "memcached", listener.Addr().String())
	raw, _ := json.Marshal(IDInput{ID: profile.ID})
	_, err := s.Execute(context.Background(), actor, "cache_status", raw)
	if core.ErrorCode(err) != "protocol_error" {
		t.Fatal("incomplete Memcached response was reported healthy", core.ErrorCode(err))
	}
	if got := <-request; got != "stats" {
		t.Fatal("unexpected Memcached command", got)
	}
}

func TestLiveCacheStatusRealServiceFixtures(t *testing.T) {
	s, admin, _, fixtures := liveDriverFixture(t)
	checked := 0
	for _, key := range []string{"redis", "redis_primary", "redis_replica", "valkey", "valkey_primary", "valkey_replica", "memcached"} {
		rawFixture := fixtures[key]
		if len(rawFixture) == 0 {
			continue
		}
		var input CacheRegisterInput
		if json.Unmarshal(rawFixture, &input) != nil {
			t.Fatalf("invalid protected %s cache fixture", key)
		}
		profile := liveAction(t, s, admin, "cache_connection_register", cacheRegistration(input)).(CacheConnection)
		token, err := s.CreateToken(context.Background(), admin, core.TokenRequest{
			Name:      "read-only cache status fixture",
			Scopes:    []string{"cache:read"},
			Resources: []string{CacheResource(profile)},
		})
		if err != nil {
			t.Fatal(err)
		}
		actor, err := s.AuthenticateToken(context.Background(), token.Token)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(IDInput{ID: profile.ID})
		data, err := s.Execute(context.Background(), actor, "cache_status", raw)
		if err != nil {
			t.Fatalf("%s status failed: %s", key, core.ErrorCode(err))
		}
		status, ok := data.(CacheStatusResult)
		if !ok || !status.Connected || status.Version == "" {
			t.Fatalf("%s returned incomplete status: %#v", key, data)
		}
		switch {
		case key == "memcached":
			if status.Memory == nil || status.Memory.UsedBytes == nil {
				t.Fatal("actual Memcached status omitted usage")
			}
		case strings.HasSuffix(key, "_replica"):
			if status.Replication == nil || status.Replication.Role != "replica" || status.Replication.LinkState == nil || *status.Replication.LinkState != "up" {
				t.Fatalf("actual %s replica status was not linked: %#v", key, status.Replication)
			}
		case strings.HasSuffix(key, "_primary"):
			if status.Replication == nil || status.Replication.Role != "master" || status.Replication.ConnectedSlaveCount == nil || *status.Replication.ConnectedSlaveCount == "0" {
				t.Fatalf("actual %s primary status omitted its replica: %#v", key, status.Replication)
			}
		default:
			if status.Memory == nil || status.Memory.UsedBytes == nil || status.Replication == nil {
				t.Fatalf("actual %s status omitted runtime fields", key)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Skip("fixture file has no Redis, Valkey, or Memcached profile")
	}
}

func setupCacheStatusProfile(t *testing.T, engine, address string) (*core.Service, core.Actor, *Manager, CacheConnection) {
	t.Helper()
	s, admin, manager := databaseManagerFixture(t)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	profile := CacheConnection{
		ID:       "cache-status-" + engine,
		Engine:   engine,
		Host:     host,
		Port:     port,
		Database: 0,
		Prefix:   "status_fixture",
		Endpoint: canonicalCacheEndpoint(address),
	}
	encoded, _ := json.Marshal(profile)
	if _, err := manager.deps.State.CompareAndSwap(context.Background(), "cache_connections", profile.ID, 0, encoded); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(context.Background(), admin, core.TokenRequest{
		Name:      "fake cache status read-only actor",
		Scopes:    []string{"cache:read"},
		Resources: []string{CacheResource(profile)},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	return s, actor, manager, profile
}

func startFakeRedisStatusServer(t *testing.T, sections map[string]string, infoDelay ...time.Duration) (net.Listener, <-chan string) {
	t.Helper()
	var delay time.Duration
	if len(infoDelay) > 0 {
		delay = infoDelay[0]
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 32)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveFakeRedisStatusConn(conn, sections, requests, delay)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, requests
}

func serveFakeRedisStatusConn(conn net.Conn, sections map[string]string, requests chan<- string, infoDelay time.Duration) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	for {
		args, err := readFakeRESPCommand(reader)
		if err != nil {
			return
		}
		if len(args) == 0 {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			_, _ = writer.WriteString("-ERR unknown command 'hello'\r\n")
		case "CLIENT":
			_, _ = writer.WriteString("-ERR unknown command 'client'\r\n")
		case "INFO":
			if len(args) != 2 {
				requests <- "invalid-info-args"
				_, _ = writer.WriteString("-ERR unexpected INFO arguments\r\n")
				break
			}
			section := strings.ToLower(args[1])
			requests <- section
			if infoDelay > 0 {
				time.Sleep(infoDelay)
			}
			body, ok := sections[section]
			if !ok {
				_, _ = writer.WriteString("-ERR unexpected INFO section\r\n")
				break
			}
			_, _ = fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(body), body)
		default:
			requests <- "unexpected-command:" + strings.ToLower(args[0])
			_, _ = writer.WriteString("-ERR unexpected command\r\n")
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func readFakeRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(header) > 32 || len(header) < 4 || header[0] != '*' || !strings.HasSuffix(header, "\r\n") {
		return nil, fmt.Errorf("invalid RESP command header")
	}
	count, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "*"), "\r\n"))
	if err != nil || count < 1 || count > 16 {
		return nil, fmt.Errorf("invalid RESP argument count")
	}
	args := make([]string, count)
	for i := range args {
		lengthLine, readErr := reader.ReadString('\n')
		if readErr != nil || len(lengthLine) > 32 || len(lengthLine) < 4 || lengthLine[0] != '$' || !strings.HasSuffix(lengthLine, "\r\n") {
			return nil, fmt.Errorf("invalid RESP bulk length")
		}
		length, parseErr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(lengthLine, "$"), "\r\n"))
		if parseErr != nil || length < 0 || length > 4096 {
			return nil, fmt.Errorf("invalid RESP bulk size")
		}
		data := make([]byte, length+2)
		if _, readErr = io.ReadFull(reader, data); readErr != nil || string(data[length:]) != "\r\n" {
			return nil, fmt.Errorf("invalid RESP bulk body")
		}
		args[i] = string(data[:length])
	}
	return args, nil
}

func startFakeMemcacheStatusServer(t *testing.T, lines []string) (net.Listener, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	request := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		command, readErr := reader.ReadString('\n')
		if readErr != nil {
			return
		}
		request <- strings.TrimSuffix(strings.TrimSuffix(command, "\n"), "\r")
		writer := bufio.NewWriter(conn)
		for _, line := range lines {
			_, _ = fmt.Fprintf(writer, "%s\r\n", line)
		}
		_, _ = writer.WriteString("END\r\n")
		_ = writer.Flush()
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, request
}
