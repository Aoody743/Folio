package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type registryFixture struct{ calls atomic.Int64 }

func (f *registryFixture) Actions(context.Context) ([]core.Action, error) {
	var actions []core.Action
	for i := range 60 {
		actions = append(actions, core.Action{Name: fmt.Sprintf("test_action_%02d", i), Description: "Integration fixture action", ReadOnly: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}})
	}
	return actions, nil
}
func (f *registryFixture) Execute(context.Context, string, json.RawMessage) (any, error) {
	f.calls.Add(1)
	return map[string]any{"ok": true}, nil
}

func TestRegistryPaginationAndSchemaBeforeExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &registryFixture{}
	server, err := newServer(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "pagination-and-schema-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	page, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tools) != ToolPageSize || page.NextCursor == "" {
		t.Fatalf("first tool page size=%d cursor=%q", len(page.Tools), page.NextCursor)
	}
	second, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tools) != 10 || second.NextCursor != "" {
		t.Fatalf("last tool page size=%d cursor=%q", len(second.Tools), second.NextCursor)
	}
	response, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: page.Tools[0].Name, Arguments: map[string]any{"actor": map[string]any{"kind": "human", "scopes": []string{"*"}}}})
	if err == nil && !response.IsError {
		t.Fatal("unknown actor arguments passed action schema")
	}
	if source.calls.Load() != 0 {
		t.Fatal("invalid input reached the business action")
	}
	response, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: page.Tools[0].Name, Arguments: map[string]any{}})
	if err != nil || response.IsError || source.calls.Load() != 1 {
		t.Fatalf("valid schema input did not execute: %v %+v", err, response)
	}
}
