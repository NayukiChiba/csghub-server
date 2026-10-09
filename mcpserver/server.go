package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewDatasetMCPServer builds the MCP server exposing the dataset search tools
// for external agent frameworks (Claude Code etc.) over streamable HTTP.
func NewDatasetMCPServer(exec *ToolExecutor) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "csghub-datasets",
		Version: "v1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_datasets",
		Description: "Search CSGHub datasets by keywords (name/description), exact tags, data modality, " +
			"natural language, repository size and sort order. Returns matching public datasets with " +
			"their namespace, name, description and tags. Use list_dataset_tags first to discover " +
			"valid tag values.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SearchDatasetsArgs) (*mcp.CallToolResult, *SearchDatasetsResult, error) {
		result, err := exec.SearchDatasets(ctx, args)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_dataset_tags",
		Description: "List the tags that actually exist on CSGHub datasets, optionally filtered by " +
			"category (task, license, framework, industry, program_language). Always call this before " +
			"using tag filters in search_datasets so tag names are real, not guessed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListDatasetTagsArgs) (*mcp.CallToolResult, *ListDatasetTagsResult, error) {
		result, err := exec.ListDatasetTags(ctx, args)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_dataset_detail",
		Description: "Get full metadata and a README preview for one dataset by namespace and name. " +
			"Use it to inspect dataset content before recommending it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args GetDatasetDetailArgs) (*mcp.CallToolResult, *DatasetDetail, error) {
		result, err := exec.GetDatasetDetail(ctx, args)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "parse_dataset_query",
		Description: "Parse a natural-language dataset search query into structured search_datasets " +
			"arguments via the server-side LLM. When the LLM is unavailable the result carries " +
			"fallback=true and the raw query as keywords.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ParseDatasetQueryArgs) (*mcp.CallToolResult, *ParseDatasetQueryResult, error) {
		result, err := exec.ParseDatasetQuery(ctx, args)
		if err != nil {
			return nil, nil, err
		}
		return nil, result, nil
	})

	return server
}
