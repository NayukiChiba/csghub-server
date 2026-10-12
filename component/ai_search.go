package component

import (
	"context"
	"fmt"
	"strings"

	"opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/mcpserver"
)

// AISearchComponent drives the dataset AI chat: a single LLM call parses the
// natural-language question into structured filters, the dataset search tools
// execute it, and the answer is composed from a template plus the structured
// dataset results — no multi-round tool-calling loop.
type AISearchComponent interface {
	// Chat answers one user message. emit is called with a text answer chunk
	// and the list of datasets actually returned by the search tools.
	Chat(ctx context.Context, req *types.AIChatRequest, emit func(types.AIChatEvent) error) error
}

type aiSearchComponentImpl struct {
	cfg  *config.Config
	exec *mcpserver.ToolExecutor
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
	return mcpserver.NewToolExecutor(dsComp, repoComp, database.NewTagStore(), llm.NewClient(), cfg, database.NewAgentConfigStore()), nil
}

func NewAISearchComponent(cfg *config.Config) (AISearchComponent, error) {
	exec, err := NewDatasetToolExecutor(cfg)
	if err != nil {
		return nil, err
	}
	return NewAISearchComponentWithDeps(cfg, exec), nil
}

// NewAISearchComponentWithDeps builds the component with explicit dependencies
// for testing.
func NewAISearchComponentWithDeps(cfg *config.Config, exec *mcpserver.ToolExecutor) AISearchComponent {
	return &aiSearchComponentImpl{cfg: cfg, exec: exec}
}

// historyTurns caps how many recent conversation turns are folded into the
// parse query so follow-ups like "那视频的呢？" keep their context.
const historyTurns = 4

func (c *aiSearchComponentImpl) Chat(ctx context.Context, req *types.AIChatRequest, emit func(types.AIChatEvent) error) error {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return fmt.Errorf("message is required")
	}

	parsed, err := c.exec.ParseDatasetQuery(ctx, mcpserver.ParseDatasetQueryArgs{
		Query: contextualQuery(req.History, message),
	})
	if err != nil || parsed == nil {
		// ParseDatasetQuery already falls back to keyword search on LLM
		// failure; this guard only covers unexpected errors.
		parsed = &mcpserver.ParseDatasetQueryResult{Keywords: message, Fallback: true}
	}

	result, err := c.exec.SearchDatasets(ctx, mcpserver.SearchDatasetsArgs{
		Keywords:     parsed.Keywords,
		Tags:         parsed.Tags,
		Modality:     parsed.Modality,
		Language:     parsed.Language,
		MinSizeBytes: parsed.MinSizeBytes,
		MaxSizeBytes: parsed.MaxSizeBytes,
		Per:          10,
	})
	if err != nil {
		return fmt.Errorf("search datasets: %w", err)
	}

	if err := emit(types.AIChatEvent{Type: types.AIChatEventText, Text: composeChatAnswer(parsed, result)}); err != nil {
		return err
	}
	if len(result.Datasets) == 0 {
		return nil
	}
	return emit(types.AIChatEvent{Type: types.AIChatEventDatasets, Datasets: result.Datasets})
}

// contextualQuery prepends recent conversation turns so the parser can resolve
// follow-up questions that omit the search target.
func contextualQuery(history []types.AIChatMessage, message string) string {
	if len(history) == 0 {
		return message
	}
	if len(history) > historyTurns {
		history = history[len(history)-historyTurns:]
	}
	var b strings.Builder
	b.WriteString("对话上下文：\n")
	for _, h := range history {
		role := "用户"
		if h.Role == "assistant" {
			role = "助手"
		} else if h.Role != "user" {
			continue
		}
		b.WriteString(role + "：" + h.Content + "\n")
	}
	b.WriteString("当前问题：" + message)
	return b.String()
}

// composeChatAnswer renders the deterministic text answer from the parse and
// search results. Dataset cards are delivered separately via the datasets
// event, so the text never links to datasets the search did not return.
func composeChatAnswer(parsed *mcpserver.ParseDatasetQueryResult, result *mcpserver.SearchDatasetsResult) string {
	var b strings.Builder
	if parsed.Fallback {
		b.WriteString("AI 解析暂不可用，已按关键词直接搜索。")
	} else if reason := strings.TrimSpace(parsed.Reason); reason != "" {
		b.WriteString("已将您的问题理解为：" + reason + "。")
	}
	switch {
	case result.Total == 0:
		b.WriteString("没有找到符合条件的数据集。建议放宽筛选条件（去掉模态、语言或大小限制），或换个关键词再试。")
	case result.Relaxed:
		fmt.Fprintf(&b, "严格筛选没有完全匹配的结果，放宽条件后为您找到 %d 个相关数据集（按相关度排序）：", result.Total)
	default:
		fmt.Fprintf(&b, "为您找到 %d 个相关数据集：", result.Total)
	}
	return b.String()
}
