package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
)

// connectInMemory wires a client to the dataset MCP server over the in-memory
// transport and returns the client session.
func connectInMemory(t *testing.T, exec *ToolExecutor) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := NewDatasetMCPServer(exec)
	_, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session
}

func TestMCPServer_ListTools(t *testing.T) {
	exec, _, _, _ := newTestExecutor()
	session := connectInMemory(t, exec)

	result, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := []string{}
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, ToolNames, names)
}

func TestMCPServer_CallSearchDatasets(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{
		Path:        "root/speech-zh",
		Name:        "speech-zh",
		Description: "Chinese speech dataset",
	}}
	ds.total = 1
	session := connectInMemory(t, exec)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_datasets",
		Arguments: map[string]any{"keywords": "speech"},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)

	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	var searchResult SearchDatasetsResult
	require.NoError(t, json.Unmarshal([]byte(text.Text), &searchResult))
	require.Equal(t, 1, searchResult.Total)
	require.Equal(t, "root/speech-zh", searchResult.Datasets[0].Namespace+"/"+searchResult.Datasets[0].Name)
}
