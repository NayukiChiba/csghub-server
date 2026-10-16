package component

import (
	"context"
	"fmt"
	"log/slog"
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
		// deterministic constraint extraction must see the current message,
		// not the folded history
		RawQuery: message,
		// earlier user turns let follow-ups inherit the constraints already
		// stated ("最大的数据集是谁" after "小于10kb的数据集")
		History: historyUserMessages(req.History),
	})
	if err != nil || parsed == nil {
		// ParseDatasetQuery already falls back to keyword search on LLM
		// failure; this guard only covers unexpected errors.
		parsed = &mcpserver.ParseDatasetQueryResult{Keywords: message, Fallback: true}
	}
	// keep the parse on record: chat requests otherwise leave no trace, so a
	// misparsed query (a wrong date, a guessed tag) cannot be investigated.
	slog.Info("ai dataset chat", slog.String("message", truncateRunes(message, 200)), slog.Any("parsed", parsed))

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
		Keywords:          parsed.Keywords,
		KeywordExpansions: parsed.KeywordExpansions,
		Tags:              parsed.Tags,
		Modality:          parsed.Modality,
		ModalityHard:      parsed.ModalityHard,
		Language:          parsed.Language,
		LanguageHard:      parsed.LanguageHard,
		MinSizeBytes:      parsed.MinSizeBytes,
		MaxSizeBytes:      parsed.MaxSizeBytes,
		CreatedAfter:      parsed.CreatedAfter,
		CreatedBefore:     parsed.CreatedBefore,
		UpdatedAfter:      parsed.UpdatedAfter,
		UpdatedBefore:     parsed.UpdatedBefore,
		Sort:              parsed.Sort,
		Per:               10,
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

// historyUserMessages returns the contents of the user turns, oldest first,
// so the parser can inherit constraints for follow-up questions.
func historyUserMessages(history []types.AIChatMessage) []string {
	var msgs []string
	for _, h := range history {
		if h.Role == "user" && strings.TrimSpace(h.Content) != "" {
			msgs = append(msgs, h.Content)
		}
	}
	return msgs
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
		parsed.CreatedBefore == "" &&
		parsed.UpdatedAfter == "" &&
		parsed.UpdatedBefore == ""
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
		if c := describeCriteria(parsed); c != "" {
			fmt.Fprintf(&b, "%s 的组合在平台上不存在，没有找到同时满足这些条件的数据集。可以减少条件或换个关键词再试。", c)
		} else {
			b.WriteString("没有找到符合条件的数据集。建议减少筛选条件，或换个关键词再试。")
		}
	default:
		fmt.Fprintf(&b, "为您找到 %d 个相关数据集%s：", result.Total, sortSuffix(parsed.Sort))
		if result.MatchMode == "tags" {
			b.WriteString("\n注：这些内容中没有直接出现您说的关键词，是按数据集标签匹配到的。")
		}
	}
	return b.String()
}

// describeCriteria renders the AND-connected criteria of a parsed query for
// the zero-result message, so the user sees exactly which combination does
// not exist instead of a generic "not found".
func describeCriteria(parsed *mcpserver.ParseDatasetQueryResult) string {
	var parts []string
	if kw := strings.TrimSpace(parsed.Keywords); kw != "" {
		parts = append(parts, "关键词「"+kw+"」")
	}
	for _, t := range parsed.Tags {
		parts = append(parts, "标签「"+t.Category+"/"+t.Name+"」")
	}
	if parsed.Modality != "" {
		parts = append(parts, "模态「"+parsed.Modality+"」")
	}
	if parsed.Language != "" {
		parts = append(parts, "语言「"+parsed.Language+"」")
	}
	if parsed.MinSizeBytes != nil {
		parts = append(parts, "大小 ≥ "+formatBytes(*parsed.MinSizeBytes))
	}
	if parsed.MaxSizeBytes != nil {
		parts = append(parts, "大小 ≤ "+formatBytes(*parsed.MaxSizeBytes))
	}
	if parsed.CreatedAfter != "" && parsed.CreatedAfter == parsed.CreatedBefore {
		parts = append(parts, "创建于 "+parsed.CreatedAfter+" 当天")
	} else {
		if parsed.CreatedAfter != "" {
			parts = append(parts, "创建于 "+parsed.CreatedAfter+" 之后")
		}
		if parsed.CreatedBefore != "" {
			parts = append(parts, "创建于 "+parsed.CreatedBefore+" 之前")
		}
	}
	if parsed.UpdatedAfter != "" && parsed.UpdatedAfter == parsed.UpdatedBefore {
		parts = append(parts, "更新于 "+parsed.UpdatedAfter+" 当天")
	} else {
		if parsed.UpdatedAfter != "" {
			parts = append(parts, "更新于 "+parsed.UpdatedAfter+" 之后")
		}
		if parsed.UpdatedBefore != "" {
			parts = append(parts, "更新于 "+parsed.UpdatedBefore+" 之前")
		}
	}
	return strings.Join(parts, " + ")
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
	case "oldest":
		return "（按创建时间从早到晚排序）"
	case "newest":
		return "（按创建时间从晚到早排序）"
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
// dataset as a fixed markdown template: a heading, the purpose paragraph, an
// attribute table (tasks/modality/license/size/downloads/creation), and a
// sample-data table when the preview conversion provides rows.
func composeDetailAnswer(d *mcpserver.DatasetDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 数据集 %s/%s\n\n", d.Namespace, d.Name)
	fmt.Fprintf(&b, "**用途**：%s\n\n", detailPurpose(d))
	b.WriteString("| 属性 | 内容 |\n|---|---|\n")
	if len(d.Tasks) > 0 {
		fmt.Fprintf(&b, "| 任务类型 | %s |\n", strings.Join(d.Tasks, "、"))
	}
	if m := modalityFromTasks(d.Tasks); m != "" {
		fmt.Fprintf(&b, "| 模态 | %s |\n", m)
	}
	license := strings.TrimSpace(d.License)
	if license == "" {
		license = "未标注"
	}
	fmt.Fprintf(&b, "| 许可 | %s |\n", license)
	fmt.Fprintf(&b, "| 数据规模 | %s |\n", detailSize(d))
	fmt.Fprintf(&b, "| 下载量 | %d 次 |\n", d.Downloads)
	fmt.Fprintf(&b, "| 创建时间 | %s |\n", d.CreatedAt)
	if len(d.SampleColumns) > 0 && len(d.SampleRows) > 0 {
		b.WriteString("\n**样例数据**：\n\n")
		b.WriteString(formatSampleTable(d.SampleColumns, d.SampleRows))
	}
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

// formatSampleTable renders sample rows as a markdown table. Cells are
// whitespace-collapsed, truncated, and pipe-escaped so a "|" in the data
// cannot break the table; rows are padded/truncated to the column count.
func formatSampleTable(cols []string, rows [][]any) string {
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = mdTableCell(c, sampleCellLimit)
	}
	writeRow(header)
	sep := make([]string, len(cols))
	for i := range sep {
		sep[i] = "---"
	}
	writeRow(sep)
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i := range cells {
			if i < len(row) {
				cells[i] = mdTableCell(fmt.Sprint(row[i]), sampleCellLimit)
			}
		}
		writeRow(cells)
	}
	return b.String()
}

// mdTableCell normalizes one value for a markdown table cell.
func mdTableCell(s string, maxRunes int) string {
	return strings.ReplaceAll(truncateRunes(s, maxRunes), "|", "\\|")
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
