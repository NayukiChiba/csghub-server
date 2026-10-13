package component

import (
	"context"
	"fmt"
	"strings"

	"opencsg.com/csghub-server/builder/dataviewer"
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
	return mcpserver.NewToolExecutor(dsComp, repoComp, database.NewTagStore(), database.NewRepositoryStatisticsStore(), dataviewer.NewDataviewerClient(cfg), llm.NewClient(), cfg, database.NewAgentConfigStore()), nil
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

	if parsed.Intent == "other" {
		return emit(types.AIChatEvent{Type: types.AIChatEventText, Text: offTopicAnswer})
	}

	// A browse intent over a message that still contains searchable terms is
	// an LLM misclassification (e.g. "中文数据集有哪些" parsed as browse with
	// empty filters); search by the raw message instead of listing all.
	if parsed.Intent == "browse" && isBrowseIntent(parsed) && mcpserver.HasSearchTerms(message) {
		parsed.Intent = "search"
		parsed.Keywords = message
	}

	if parsed.Intent == "tags" {
		tags, err := c.exec.ListDatasetTags(ctx, mcpserver.ListDatasetTagsArgs{})
		if err != nil {
			return fmt.Errorf("list dataset tags: %w", err)
		}
		return emit(types.AIChatEvent{Type: types.AIChatEventText, Text: composeTagsAnswer(tags)})
	}

	if parsed.Intent == "detail" && strings.TrimSpace(parsed.Dataset) != "" {
		return c.chatDatasetDetail(ctx, parsed.Dataset, emit)
	}

	result, err := c.exec.SearchDatasets(ctx, mcpserver.SearchDatasetsArgs{
		Keywords:      parsed.Keywords,
		Tags:          parsed.Tags,
		Modality:      parsed.Modality,
		Language:      parsed.Language,
		MinSizeBytes:  parsed.MinSizeBytes,
		MaxSizeBytes:  parsed.MaxSizeBytes,
		CreatedAfter:  parsed.CreatedAfter,
		CreatedBefore: parsed.CreatedBefore,
		Sort:          parsed.Sort,
		Per:           10,
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

// isBrowseIntent reports whether the parser returned no criteria at all —
// the user wants to browse or list datasets (e.g. "有哪些数据集"). The
// intent label alone is not trusted: a misclassification like intent=browse
// with keywords="视频数据集" must still search by those keywords.
func isBrowseIntent(parsed *mcpserver.ParseDatasetQueryResult) bool {
	return !parsed.Fallback &&
		strings.TrimSpace(parsed.Keywords) == "" &&
		len(parsed.Tags) == 0 &&
		parsed.Modality == "" &&
		parsed.Language == "" &&
		parsed.MinSizeBytes == nil &&
		parsed.MaxSizeBytes == nil &&
		parsed.CreatedAfter == "" &&
		parsed.CreatedBefore == ""
}

// composeChatAnswer renders the deterministic text answer from the parse and
// search results. Dataset cards are delivered separately via the datasets
// event, so the text never links to datasets the search did not return.
func composeChatAnswer(parsed *mcpserver.ParseDatasetQueryResult, result *mcpserver.SearchDatasetsResult) string {
	var b strings.Builder
	if parsed.Fallback {
		b.WriteString("AI 解析暂不可用，已按关键词直接搜索。")
	} else if reason := strings.TrimSpace(parsed.Reason); reason != "" && !isBrowseIntent(parsed) {
		b.WriteString("已将您的问题理解为：" + strings.TrimRight(reason, "。.!！") + "。")
	}
	switch {
	case isBrowseIntent(parsed) && result.Total == 0:
		b.WriteString("平台上目前还没有数据集。")
	case isBrowseIntent(parsed):
		fmt.Fprintf(&b, "平台目前共有 %d 个数据集%s：", result.Total, sortSuffix(parsed.Sort))
	case result.Total == 0:
		b.WriteString("没有找到符合条件的数据集。建议放宽筛选条件（去掉模态、语言或大小限制），或换个关键词再试。")
	case result.Relaxed:
		fmt.Fprintf(&b, "严格筛选没有完全匹配的结果，放宽条件后为您找到 %d 个相关数据集（按相关度排序）：", result.Total)
	default:
		fmt.Fprintf(&b, "为您找到 %d 个相关数据集%s：", result.Total, sortSuffix(parsed.Sort))
	}
	return b.String()
}

// sortSuffix notes the active ordering in the answer so "最大的数据集" is
// visibly answered by size, not by an unexplained order.
func sortSuffix(sort string) string {
	switch sort {
	case "largest":
		return "（按体积从大到小排序）"
	case "smallest":
		return "（按体积从小到大排序）"
	case "most_download":
		return "（按下载量排序）"
	case "most_favorite":
		return "（按收藏量排序）"
	case "most_star":
		return "（按星标数排序）"
	case "recently_update":
		return "（按最近更新排序）"
	default:
		return ""
	}
}

// offTopicAnswer is the fixed refusal for messages unrelated to datasets
// (intent "other"); the chat never searches or lists datasets for these.
const offTopicAnswer = "我是数据集搜索助手，只能帮你查找和筛选数据集哦～可以试试问我：「现在有哪些数据集」「语音数据集」「数据集有哪些标签」。"

// chatDatasetDetail answers questions about one specific dataset: it resolves
// the (possibly bare) dataset name, loads its metadata plus the converted
// preview, and composes a structured summary. The dataset card event lets the
// frontend link straight to the dataset page.
func (c *aiSearchComponentImpl) chatDatasetDetail(ctx context.Context, ref string, emit func(types.AIChatEvent) error) error {
	notFound := types.AIChatEvent{Type: types.AIChatEventText, Text: fmt.Sprintf("没有找到名为「%s」的数据集。可以试试完整的「命名空间/名称」，或者告诉我关键词让我帮你搜索。", strings.TrimSpace(ref))}
	namespace, name, err := c.resolveDatasetRef(ctx, ref)
	if err != nil {
		return emit(notFound)
	}
	detail, err := c.exec.GetDatasetDetail(ctx, mcpserver.GetDatasetDetailArgs{Namespace: namespace, Name: name})
	if err != nil {
		// An explicit namespace/name that does not exist may be a namespace the
		// LLM guessed; retry resolving the bare name through search.
		if _, bare, ok := strings.Cut(strings.TrimSpace(ref), "/"); ok && strings.TrimSpace(bare) != "" {
			if ns, nm, rerr := c.resolveDatasetRef(ctx, bare); rerr == nil {
				if detail, err = c.exec.GetDatasetDetail(ctx, mcpserver.GetDatasetDetailArgs{Namespace: ns, Name: nm}); err == nil {
					return c.emitDatasetDetail(detail, emit)
				}
			}
		}
		return emit(notFound)
	}
	return c.emitDatasetDetail(detail, emit)
}

func (c *aiSearchComponentImpl) emitDatasetDetail(detail *mcpserver.DatasetDetail, emit func(types.AIChatEvent) error) error {
	if err := emit(types.AIChatEvent{Type: types.AIChatEventText, Text: composeDetailAnswer(detail)}); err != nil {
		return err
	}
	return emit(types.AIChatEvent{Type: types.AIChatEventDatasets, Datasets: []types.DatasetSummary{detail.DatasetSummary}})
}

// resolveDatasetRef turns a detail-intent dataset reference into a
// namespace/name pair. A "namespace/name" reference is used as-is; a bare
// name is resolved through search, preferring an exact name match.
func (c *aiSearchComponentImpl) resolveDatasetRef(ctx context.Context, ref string) (string, string, error) {
	ref = strings.TrimSpace(strings.Trim(ref, "「」\"'`"))
	if ns, name, ok := strings.Cut(ref, "/"); ok {
		if ns = strings.TrimSpace(ns); ns != "" {
			if name = strings.TrimSpace(name); name != "" {
				return ns, name, nil
			}
		}
	}
	if ref == "" {
		return "", "", fmt.Errorf("empty dataset reference")
	}
	result, err := c.exec.SearchDatasets(ctx, mcpserver.SearchDatasetsArgs{Keywords: ref, Per: defaultPer})
	if err != nil {
		return "", "", fmt.Errorf("resolve dataset %q: %w", ref, err)
	}
	for i := range result.Datasets {
		if strings.EqualFold(result.Datasets[i].Name, ref) {
			return result.Datasets[i].Namespace, result.Datasets[i].Name, nil
		}
	}
	if len(result.Datasets) > 0 {
		return result.Datasets[0].Namespace, result.Datasets[0].Name, nil
	}
	return "", "", fmt.Errorf("dataset %q not found", ref)
}

// defaultPer is the page size for name-resolution searches in detail flow.
const defaultPer = 10

// detailPurposePreview caps how many readme runes stand in for a missing
// description in the detail answer.
const detailPurposePreview = 200

// composeDetailAnswer renders the structured basic-info answer for one
// dataset: purpose, tasks/modality, license, size and a few sample rows.
func composeDetailAnswer(d *mcpserver.DatasetDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "数据集 %s/%s 的基本信息：", d.Namespace, d.Name)
	b.WriteString("\n用途：" + detailPurpose(d))
	if len(d.Tasks) > 0 {
		b.WriteString("\n任务类型：" + strings.Join(d.Tasks, "、"))
	}
	if m := modalityFromTasks(d.Tasks); m != "" {
		b.WriteString("\n模态：" + m)
	}
	license := strings.TrimSpace(d.License)
	if license == "" {
		license = "未标注"
	}
	b.WriteString("\n许可：" + license)
	b.WriteString("\n数据规模：" + detailSize(d))
	if len(d.SampleColumns) > 0 && len(d.SampleRows) > 0 {
		b.WriteString("\n样例数据：")
		for _, row := range d.SampleRows {
			b.WriteString("\n" + formatSampleRow(d.SampleColumns, row))
		}
	}
	fmt.Fprintf(&b, "\n下载量 %d 次 · 创建于 %s", d.Downloads, d.CreatedAt)
	return b.String()
}

func detailPurpose(d *mcpserver.DatasetDetail) string {
	if desc := strings.TrimSpace(d.Description); desc != "" {
		return desc
	}
	if readme := strings.TrimSpace(d.ReadmePreview); readme != "" {
		return truncateRunes(readme, detailPurposePreview)
	}
	return "（暂无描述）"
}

// maxSplitsInDetail caps how many splits the size line lists.
const maxSplitsInDetail = 6

func detailSize(d *mcpserver.DatasetDetail) string {
	var parts []string
	if d.Size > 0 {
		parts = append(parts, "存储体积 "+formatBytes(d.Size))
	}
	if d.RowsNum > 0 {
		parts = append(parts, fmt.Sprintf("共 %d 行", d.RowsNum))
	}
	splits := d.Splits
	if len(splits) > maxSplitsInDetail {
		splits = splits[:maxSplitsInDetail]
	}
	for _, sp := range splits {
		parts = append(parts, fmt.Sprintf("%s/%s 共 %d 行", sp.Config, sp.Name, sp.Rows))
	}
	if len(d.Splits) > maxSplitsInDetail {
		parts = append(parts, "等")
	}
	if len(parts) == 0 {
		return "（暂未统计）"
	}
	return strings.Join(parts, "；")
}

// sampleCellLimit caps one sample cell so long texts stay readable in chat.
const sampleCellLimit = 60

func formatSampleRow(cols []string, row []any) string {
	cells := make([]string, 0, len(row))
	for i, v := range row {
		cell := truncateRunes(fmt.Sprint(v), sampleCellLimit)
		if i < len(cols) {
			cell = cols[i] + ": " + cell
		}
		cells = append(cells, cell)
	}
	return strings.Join(cells, " | ")
}

func truncateRunes(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// taskModalityNames maps task-tag prefixes to display modality names.
var taskModalityNames = map[string]string{
	"image": "图像",
	"audio": "音频",
	"video": "视频",
	"text":  "文本",
	"3d":    "3D",
}

func modalityFromTasks(tasks []string) string {
	var mods []string
	seen := make(map[string]bool)
	for _, t := range tasks {
		prefix, _, _ := strings.Cut(t, "-")
		if name, ok := taskModalityNames[prefix]; ok && !seen[name] {
			seen[name] = true
			mods = append(mods, name)
		}
	}
	return strings.Join(mods, "、")
}

var byteUnitNames = []string{"Bytes", "KB", "MB", "GB", "TB", "PB"}

func formatBytes(size int64) string {
	f := float64(size)
	i := 0
	for f >= 1024 && i < len(byteUnitNames)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d Bytes", size)
	}
	return fmt.Sprintf("%.1f %s", f, byteUnitNames[i])
}

// tagCategoryDisplayNames maps tag categories to Chinese display names.
var tagCategoryDisplayNames = map[string]string{
	"task":             "任务类型",
	"license":          "许可证",
	"framework":        "框架",
	"industry":         "行业",
	"program_language": "编程语言",
}

// maxTagNamesPerCategory caps how many tag names one category lists in the
// chat answer; the remainder is summarized as "等".
const maxTagNamesPerCategory = 15

// composeTagsAnswer renders the answer for "which tags exist" questions from
// the real tag vocabulary, grouped by category.
func composeTagsAnswer(result *mcpserver.ListDatasetTagsResult) string {
	if len(result.Tags) == 0 {
		return "平台上还没有配置数据集标签。"
	}
	byCategory := make(map[string][]string)
	var order []string
	for _, t := range result.Tags {
		if _, ok := byCategory[t.Category]; !ok {
			order = append(order, t.Category)
		}
		byCategory[t.Category] = append(byCategory[t.Category], t.Name)
	}
	var b strings.Builder
	b.WriteString("平台的数据集标签按类别划分如下，可以直接让我按标签筛选：")
	for _, cat := range order {
		names := byCategory[cat]
		display := cat
		if name, ok := tagCategoryDisplayNames[cat]; ok {
			display = name
		}
		fmt.Fprintf(&b, "\n%s（共 %d 个）：", display, len(names))
		shown := names
		if len(shown) > maxTagNamesPerCategory {
			shown = shown[:maxTagNamesPerCategory]
		}
		b.WriteString(strings.Join(shown, "、"))
		if len(names) > maxTagNamesPerCategory {
			b.WriteString(" 等")
		}
	}
	return b.String()
}
