package component

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/mcpserver"
)

// AISearchComponent drives the dataset AI chat: an LLM agent loop that calls
// the dataset search tools and streams text plus structured dataset results
// back through emit.
type AISearchComponent interface {
	// Chat answers one user message. emit is called with text chunks and the
	// final list of datasets actually returned by the search tools.
	Chat(ctx context.Context, req *types.AIChatRequest, emit func(types.AIChatEvent) error) error
}

type aiSearchComponentImpl struct {
	cfg   *config.Config
	exec  *mcpserver.ToolExecutor
	llm   llm.LLMSvcClient
	tools []types.LLMTool
}

// NewDatasetToolExecutor builds the shared dataset search tool executor. It is
// used by the AI chat component and by the router to serve the MCP endpoint.
func NewDatasetToolExecutor(cfg *config.Config) (*mcpserver.ToolExecutor, error) {
	dsComp, err := NewDatasetComponent(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dataset component: %w", err)
	}
	repoComp, err := NewRepoComponent(cfg)
	if err != nil {
		return nil, fmt.Errorf("create repo component: %w", err)
	}
	return mcpserver.NewToolExecutor(dsComp, repoComp, database.NewTagStore(), llm.NewClient(), cfg), nil
}

func NewAISearchComponent(cfg *config.Config) (AISearchComponent, error) {
	exec, err := NewDatasetToolExecutor(cfg)
	if err != nil {
		return nil, err
	}
	return NewAISearchComponentWithDeps(cfg, exec, llm.NewClient()), nil
}

// NewAISearchComponentWithDeps builds the component with explicit dependencies
// for testing.
func NewAISearchComponentWithDeps(cfg *config.Config, exec *mcpserver.ToolExecutor, llmClient llm.LLMSvcClient) AISearchComponent {
	return &aiSearchComponentImpl{
		cfg:   cfg,
		exec:  exec,
		llm:   llmClient,
		tools: aiSearchToolDefs(),
	}
}

const aiSearchSystemPrompt = `你是 CSGHub 数据集搜索助手，帮助用户用自然语言查找平台上的数据集。

规则（必须严格遵守）：
1. 只能通过调用工具获取数据集信息，绝不能凭记忆或想象推荐数据集。
2. 只允许推荐工具返回结果中真实存在的数据集，引用格式为 namespace/name。
3. 工具没有返回结果时，明确告诉用户"没有找到符合条件的数据集"，并建议放宽筛选条件或更换关键词。
4. 与数据集搜索无关的问题，礼貌地说明你只能帮助搜索数据集，不要回答其他内容。
5. 筛选标签前先调用 list_dataset_tags 了解真实存在的标签值，不要编造标签名。
6. 需要了解数据集内容细节时调用 get_dataset_detail 查看 README。
7. 回答使用与用户相同的语言。`

func (c *aiSearchComponentImpl) Chat(ctx context.Context, req *types.AIChatRequest, emit func(types.AIChatEvent) error) error {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return fmt.Errorf("message is required")
	}

	if c.cfg.AISearch.LLMEndpoint == "" {
		slog.Info("ai search llm not configured, fallback to keyword search")
		return c.keywordFallback(ctx, message, emit)
	}

	messages := []types.LLMMessage{{Role: "system", Content: aiSearchSystemPrompt}}
	for _, h := range req.History {
		role := h.Role
		if role != "user" && role != "assistant" {
			continue
		}
		messages = append(messages, types.LLMMessage{Role: role, Content: h.Content})
	}
	messages = append(messages, types.LLMMessage{Role: "user", Content: message})

	maxIter := c.cfg.AISearch.MaxToolIter
	if maxIter <= 0 {
		maxIter = 5
	}
	timeout := time.Duration(c.cfg.AISearch.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	headers := map[string]string{}
	if c.cfg.AISearch.LLMAPIKey != "" {
		headers["Authorization"] = "Bearer " + c.cfg.AISearch.LLMAPIKey
	}

	// found tracks datasets actually returned by search_datasets, so the final
	// answer can only reference real data.
	found := map[string]types.DatasetSummary{}
	var order []string

	for range maxIter {
		llmCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := c.llm.ChatCompletion(llmCtx, c.cfg.AISearch.LLMEndpoint, "", headers, types.LLMReqBody{
			Model:    c.cfg.AISearch.LLMModel,
			Messages: messages,
			Tools:    c.tools,
		})
		cancel()
		if err != nil {
			slog.Warn("ai search llm call failed, fallback to keyword search", slog.Any("error", err))
			return c.keywordFallback(ctx, message, emit)
		}
		if len(resp.Choices) == 0 {
			slog.Warn("ai search llm returned no choices, fallback to keyword search")
			return c.keywordFallback(ctx, message, emit)
		}

		choice := resp.Choices[0].Message
		slog.Info("ai search llm round", slog.String("content", choice.Content), slog.Any("tool_calls", choice.ToolCalls))
		if len(choice.ToolCalls) == 0 {
			if strings.TrimSpace(choice.Content) == "" {
				return c.keywordFallback(ctx, message, emit)
			}
			if err := emit(types.AIChatEvent{Type: types.AIChatEventText, Text: choice.Content}); err != nil {
				return err
			}
			return emitDatasets(found, order, emit)
		}

		messages = append(messages, types.LLMMessage{Role: "assistant", Content: choice.Content, ToolCalls: choice.ToolCalls})
		for _, call := range choice.ToolCalls {
			output, err := c.exec.Execute(ctx, call.Function.Name, call.Function.Arguments)
			if err != nil {
				slog.Warn("ai search tool call failed", slog.String("tool", call.Function.Name), slog.Any("error", err))
				output = fmt.Sprintf(`{"error":%q}`, err.Error())
			}
			if call.Function.Name == "search_datasets" {
				var result mcpserver.SearchDatasetsResult
				if json.Unmarshal([]byte(output), &result) == nil {
					for _, ds := range result.Datasets {
						key := ds.Namespace + "/" + ds.Name
						if _, ok := found[key]; !ok {
							order = append(order, key)
						}
						found[key] = ds
					}
				}
			}
			messages = append(messages, types.LLMMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    output,
			})
		}
	}

	slog.Warn("ai search reached max tool iterations")
	if err := emit(types.AIChatEvent{Type: types.AIChatEventText, Text: "搜索条件较复杂，以下是目前找到的结果，你可以换个问法继续。"}); err != nil {
		return err
	}
	return emitDatasets(found, order, emit)
}

func emitDatasets(found map[string]types.DatasetSummary, order []string, emit func(types.AIChatEvent) error) error {
	if len(order) == 0 {
		return nil
	}
	datasets := make([]types.DatasetSummary, 0, len(order))
	for _, key := range order {
		datasets = append(datasets, found[key])
	}
	return emit(types.AIChatEvent{Type: types.AIChatEventDatasets, Datasets: datasets})
}

// keywordFallback answers with plain keyword search results when the LLM is
// unavailable, so the chat still works without any AI service.
func (c *aiSearchComponentImpl) keywordFallback(ctx context.Context, message string, emit func(types.AIChatEvent) error) error {
	result, err := c.exec.SearchDatasets(ctx, mcpserver.SearchDatasetsArgs{Keywords: message, Per: 10})
	if err != nil {
		return fmt.Errorf("keyword fallback search: %w", err)
	}
	var text string
	if result.Total == 0 {
		text = fmt.Sprintf("AI 服务暂不可用，已按关键词 %q 搜索，没有找到符合条件的数据集。你可以换个关键词再试。", message)
	} else {
		text = fmt.Sprintf("AI 服务暂不可用，已按关键词 %q 搜索，共找到 %d 个数据集：", message, result.Total)
	}
	if err := emit(types.AIChatEvent{Type: types.AIChatEventText, Text: text}); err != nil {
		return err
	}
	found := map[string]types.DatasetSummary{}
	var order []string
	for _, ds := range result.Datasets {
		key := ds.Namespace + "/" + ds.Name
		found[key] = ds
		order = append(order, key)
	}
	return emitDatasets(found, order, emit)
}

// aiSearchToolDefs returns the OpenAI-style tool definitions for the agent
// loop. Keep in sync with the argument structs in mcpserver/tools.go.
func aiSearchToolDefs() []types.LLMTool {
	return []types.LLMTool{
		{
			Type: "function",
			Function: types.LLMToolFunction{
				Name: "search_datasets",
				Description: "Search CSGHub datasets by keywords, exact tags, modality, language and size. " +
					"Only datasets returned by this tool may be recommended to the user.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"keywords":       map[string]any{"type": "string", "description": "space-separated keywords matched against dataset name and description"},
						"modality":       map[string]any{"type": "string", "enum": []string{"image", "audio", "video", "text", "3d"}, "description": "data modality filter"},
						"language":       map[string]any{"type": "string", "description": "natural language of dataset content, e.g. chinese, english"},
						"min_size_bytes": map[string]any{"type": "integer", "description": "minimum repository size in bytes"},
						"max_size_bytes": map[string]any{"type": "integer", "description": "maximum repository size in bytes"},
						"sort":           map[string]any{"type": "string", "enum": []string{"trending", "recently_update", "most_download", "most_favorite", "most_star"}},
						"page":           map[string]any{"type": "integer", "description": "page number starting at 1"},
						"per":            map[string]any{"type": "integer", "description": "page size, max 50"},
						"tags": map[string]any{
							"type":        "array",
							"description": "exact tag filters, AND-combined; discover valid values with list_dataset_tags",
							"items": map[string]any{
								"type":       "object",
								"properties": map[string]any{"category": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "group": map[string]any{"type": "string"}},
								"required":   []string{"category", "name"},
							},
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: types.LLMToolFunction{
				Name:        "list_dataset_tags",
				Description: "List tags that actually exist on CSGHub datasets, optionally filtered by category (task, license, framework, industry, program_language).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: types.LLMToolFunction{
				Name:        "get_dataset_detail",
				Description: "Get full metadata and a README preview for one dataset by namespace and name.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"namespace": map[string]any{"type": "string"},
						"name":      map[string]any{"type": "string"},
					},
					"required": []string{"namespace", "name"},
				},
			},
		},
		{
			Type: "function",
			Function: types.LLMToolFunction{
				Name:        "parse_dataset_query",
				Description: "Parse a natural-language dataset search query into structured search_datasets arguments.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}
