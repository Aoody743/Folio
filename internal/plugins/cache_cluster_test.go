package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	clusterFixtureNodeA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	clusterFixtureNodeB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	clusterFixtureNodeC = "cccccccccccccccccccccccccccccccccccccccc"
	clusterFixtureNodeD = "dddddddddddddddddddddddddddddddddddddddd"
)

func TestCacheClusterActionsUseRedisProtocolAndOmitPeerAddresses(t *testing.T) {
	info := "# Cluster\r\ncluster_state:ok\r\ncluster_slots_assigned:16384\r\ncluster_slots_ok:16384\r\ncluster_slots_pfail:0\r\ncluster_slots_fail:0\r\ncluster_known_nodes:4\r\ncluster_size:3\r\ncluster_current_epoch:9\r\ncluster_my_epoch:2\r\nredis_password:private-cluster-password\r\n"
	nodes := strings.Join([]string{
		clusterFixtureNodeA + " private-a.example:7000@17000 myself,master - 0 10 2 connected 0-5461 [5461->-" + clusterFixtureNodeB + "]",
		clusterFixtureNodeB + " private-b.example:7000@17000 master - 0 8 2 connected 5462-10922",
		clusterFixtureNodeC + " private-c.example:7000@17000 master - 0 6 2 connected 10923-16383",
		clusterFixtureNodeD + " private-d.example:7000@17000 slave " + clusterFixtureNodeA + " 0 4 2 connected",
	}, "\r\n") + "\r\n"
	listener, requests := startFakeRedisClusterServer(t, info, nodes, false)
	_, actor, manager, profile := setupCacheStatusProfile(t, "redis", listener.Addr().String())
	input, _ := json.Marshal(IDInput{ID: profile.ID})

	infoResult, err := manager.cacheClusterInfo(context.Background(), actor, input)
	if err != nil {
		t.Fatalf("read cluster info failed safely: %s", core.ErrorCode(err))
	}
	clusterInfo, ok := infoResult.Data.(CacheClusterInfo)
	if !ok || clusterInfo.State != "ok" || clusterInfo.SlotsAssigned != 16384 || clusterInfo.KnownNodes != 4 || clusterInfo.Size != 3 || clusterInfo.CurrentEpoch == nil || *clusterInfo.CurrentEpoch != "9" {
		t.Fatalf("typed cluster info mismatch: %#v", infoResult.Data)
	}
	if len(infoResult.Resources) != 1 || infoResult.Resources[0] != CacheResource(profile) {
		t.Fatalf("cluster info was not scoped to the exact cache namespace: %#v", infoResult.Resources)
	}
	nodesResult, err := manager.cacheClusterNodes(context.Background(), actor, input)
	if err != nil {
		t.Fatalf("read cluster nodes failed safely: %s", core.ErrorCode(err))
	}
	clusterNodes, ok := nodesResult.Data.(CacheClusterNodes)
	if !ok || len(clusterNodes.Nodes) != 4 || clusterNodes.Nodes[0].NodeID != clusterFixtureNodeA || clusterNodes.Nodes[0].Role != "master" || !clusterNodes.Nodes[0].Self || len(clusterNodes.Nodes[0].Transitions) != 1 || clusterNodes.Nodes[0].Transitions[0].Direction != "migrating" || clusterNodes.Nodes[3].Role != "replica" {
		t.Fatalf("typed cluster nodes mismatch: %#v", nodesResult.Data)
	}
	if clusterNodes.Nodes[0].Slots[0] != (CacheClusterSlotRange{Start: 0, End: 5461}) {
		t.Fatalf("cluster slot range mismatch: %#v", clusterNodes.Nodes[0].Slots)
	}
	for _, want := range []string{"CLUSTER INFO", "CLUSTER NODES"} {
		select {
		case got := <-requests:
			if got != want {
				t.Fatalf("unexpected Redis cluster command %q, wanted %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("Redis did not receive %s", want)
		}
	}
	encoded, err := json.Marshal(map[string]any{"info": clusterInfo, "nodes": clusterNodes})
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	for _, private := range []string{"private-a.example", "private-b.example", "private-c.example", "private-d.example", "private-cluster-password", "redis_password", "private-a"} {
		if strings.Contains(public, private) {
			t.Fatalf("cluster result leaked private topology or raw response field %q: %s", private, public)
		}
	}
}

func TestCacheClusterDisabledAndUnsupportedEngineAreExplicit(t *testing.T) {
	listener, requests := startFakeRedisClusterServer(t, "", "", true)
	_, actor, manager, profile := setupCacheStatusProfile(t, "valkey", listener.Addr().String())
	input, _ := json.Marshal(IDInput{ID: profile.ID})
	_, err := manager.cacheClusterInfo(context.Background(), actor, input)
	if core.ErrorCode(err) != "cluster_disabled" || strings.Contains(err.Error(), "private") {
		t.Fatalf("disabled Valkey cluster did not return an explicit safe result: %s %v", core.ErrorCode(err), err)
	}
	select {
	case got := <-requests:
		if got != "CLUSTER INFO" {
			t.Fatalf("unexpected command for disabled cluster: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("disabled cluster command was not sent")
	}

	memcached := CacheConnection{ID: "cache-cluster-memcached", Engine: "memcached", Host: "127.0.0.1", Port: 11211, Prefix: "cluster_fixture"}
	encoded, _ := json.Marshal(memcached)
	if _, err := manager.deps.State.CompareAndSwap(context.Background(), "cache_connections", memcached.ID, 0, encoded); err != nil {
		t.Fatal(err)
	}
	memInput, _ := json.Marshal(IDInput{ID: memcached.ID})
	if _, err := manager.cacheClusterNodes(context.Background(), actor, memInput); core.ErrorCode(err) != "unsupported_operation" {
		t.Fatalf("Memcached cluster action was not rejected: %s", core.ErrorCode(err))
	}
}

func TestRedisClusterParsersRejectMalformedAndOversizedViews(t *testing.T) {
	validInfo := "cluster_state:ok\ncluster_slots_assigned:0\ncluster_slots_ok:0\ncluster_slots_pfail:0\ncluster_slots_fail:0\ncluster_known_nodes:1\ncluster_size:1\n"
	for name, raw := range map[string]string{
		"oversized info":        strings.Repeat("x", maxRedisClusterInfoBytes+1),
		"duplicate field":       validInfo + "cluster_size:1\n",
		"inconsistent counters": strings.Replace(validInfo, "cluster_slots_ok:0", "cluster_slots_ok:1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRedisClusterInfo(raw); err == nil {
				t.Fatal("invalid cluster info was accepted")
			}
		})
	}
	validNode := clusterFixtureNodeA + " 10.0.0.1:7000@17000 myself,master - 0 0 1 connected 0-10\n"
	for name, raw := range map[string]string{
		"oversized nodes": strings.Repeat("x", maxRedisClusterNodesBytes+1),
		"invalid id":      strings.Replace(validNode, clusterFixtureNodeA, "not-a-node-id", 1),
		"invalid slot":    strings.Replace(validNode, "0-10", "0-16384", 1),
		"unknown flag":    strings.Replace(validNode, "myself,master", "myself,master,private", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRedisClusterNodes(raw); err == nil {
				t.Fatal("invalid cluster nodes were accepted")
			}
		})
	}
}

func startFakeRedisClusterServer(t *testing.T, info, nodes string, disabled bool) (net.Listener, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 8)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveFakeRedisClusterConnection(connection, requests, info, nodes, disabled)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, requests
}

func serveFakeRedisClusterConnection(connection net.Conn, requests chan<- string, info, nodes string, disabled bool) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	for {
		args, err := readFakeRESPCommand(reader)
		if err != nil {
			return
		}
		if len(args) == 0 {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO", "CLIENT":
			_, _ = writer.WriteString("-ERR unsupported command\r\n")
		case "CLUSTER":
			if len(args) != 2 {
				_, _ = writer.WriteString("-ERR invalid cluster arguments\r\n")
				break
			}
			operation := strings.ToUpper(args[1])
			requests <- "CLUSTER " + operation
			if disabled {
				_, _ = writer.WriteString("-ERR This instance has cluster support disabled\r\n")
				break
			}
			body := ""
			switch operation {
			case "INFO":
				body = info
			case "NODES":
				body = nodes
			default:
				_, _ = writer.WriteString("-ERR unsupported cluster operation\r\n")
				break
			}
			_, _ = fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(body), body)
		default:
			_, _ = writer.WriteString("-ERR unexpected command\r\n")
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}
