// Package mcpserver exposes CSGHub dataset search as MCP tools so external
// agent frameworks (e.g. Claude Code) can answer natural-language dataset
// questions. The same tool executor is also driven by the in-process AI chat
// agent loop (component/ai_search.go).
package mcpserver

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
)

// Narrow dependency interfaces: satisfied by component implementations without
// importing the component package (which would create an import cycle).

// DatasetSearcher searches datasets; implemented by component.DatasetComponent.
type DatasetSearcher interface {
	Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error)
	Show(ctx context.Context, namespace, name, currentUser string, needOpWeight, needMultiSync bool) (*types.Dataset, error)
}

// RepoFileReader reads raw file content; implemented by component.RepoComponent.
type RepoFileReader interface {
	FileRaw(ctx context.Context, req *types.GetFileReq) (string, error)
}

// DatasetTagLister lists dataset-scope tags; implemented by database.TagStore.
type DatasetTagLister interface {
	AllDatasetTags(ctx context.Context) ([]*database.Tag, error)
}

const (
	// readmePreviewLimit caps how many readme bytes get_dataset_detail returns.
	readmePreviewLimit = 4000
	maxPerPage         = 50
	defaultPerPage     = 10
)

// ToolExecutor executes the dataset search tools. It is shared by the MCP
// server and the in-process chat agent loop.
type ToolExecutor struct {
	datasets     DatasetSearcher
	repo         RepoFileReader
	tags         DatasetTagLister
	llm          llm.LLMSvcClient
	cfg          *config.Config
	agentConfigs database.AgentConfigStore
}

func NewToolExecutor(datasets DatasetSearcher, repo RepoFileReader, tags DatasetTagLister, llmClient llm.LLMSvcClient, cfg *config.Config, agentConfigs database.AgentConfigStore) *ToolExecutor {
	return &ToolExecutor{
		datasets:     datasets,
		repo:         repo,
		tags:         tags,
		llm:          llmClient,
		cfg:          cfg,
		agentConfigs: agentConfigs,
	}
}

// AISearchLLMConfigName is the agent_configs row holding the admin-editable
// LLM settings for dataset AI search. Values present there override the
// env/toml AISearch config.
const AISearchLLMConfigName = "ai_search_llm"

const (
	llmConfigKeyEndpoint = "llm_endpoint"
	llmConfigKeyModel    = "llm_model"
	llmConfigKeyAPIKey   = "llm_api_key"
)

// ResolveLLMConfig returns the effective LLM settings for AI search: values
// stored in agent_configs (set via the admin API) override the static
// env/toml configuration field by field.
func (e *ToolExecutor) ResolveLLMConfig(ctx context.Context) (endpoint, model, apiKey string) {
	endpoint, model, apiKey = e.cfg.AISearch.LLMEndpoint, e.cfg.AISearch.LLMModel, e.cfg.AISearch.LLMAPIKey
	if e.agentConfigs == nil {
		return endpoint, model, apiKey
	}
	row, err := e.agentConfigs.GetByName(ctx, AISearchLLMConfigName)
	if err != nil {
		slog.Warn("ai search llm config lookup failed, using static config", slog.Any("error", err))
		return endpoint, model, apiKey
	}
	if row == nil {
		return endpoint, model, apiKey
	}
	if v, ok := row.Config[llmConfigKeyEndpoint].(string); ok && v != "" {
		endpoint = v
	}
	if v, ok := row.Config[llmConfigKeyModel].(string); ok && v != "" {
		model = v
	}
	if v, ok := row.Config[llmConfigKeyAPIKey].(string); ok && v != "" {
		apiKey = v
	}
	return endpoint, model, apiKey
}

// ToolNames lists all registered tool names.
var ToolNames = []string{"search_datasets", "list_dataset_tags", "get_dataset_detail", "parse_dataset_query"}

// ---------- search_datasets ----------

type TagArg struct {
	Category string `json:"category" jsonschema:"tag category, e.g. task, license, framework, industry, program_language"`
	Name     string `json:"name" jsonschema:"exact tag name; call list_dataset_tags first to discover valid values"`
	Group    string `json:"group,omitempty" jsonschema:"optional tag group"`
}

type SearchDatasetsArgs struct {
	Keywords     string   `json:"keywords,omitempty" jsonschema:"search terms matched against dataset name and description; space-separated, chinese or english; synonyms are expanded automatically"`
	Tags         []TagArg `json:"tags,omitempty" jsonschema:"exact tag filters (AND-combined); discover valid values with list_dataset_tags"`
	Modality     string   `json:"modality,omitempty" jsonschema:"data modality filter: image, audio, video, text, 3d or multimodal; expanded to synonyms and matched against name and description"`
	Language     string   `json:"language,omitempty" jsonschema:"natural language of the dataset content, e.g. chinese, english, 中文; expanded to synonyms"`
	MinSizeBytes *int64   `json:"min_size_bytes,omitempty" jsonschema:"minimum repository size in bytes; omit unless the user specifies a size"`
	MaxSizeBytes *int64   `json:"max_size_bytes,omitempty" jsonschema:"maximum repository size in bytes; omit unless the user specifies a size"`
	Sort         string   `json:"sort,omitempty" jsonschema:"sort order: trending, recently_update, most_download, most_favorite or most_star"`
	Page         int      `json:"page,omitempty" jsonschema:"page number, starting at 1"`
	Per          int      `json:"per,omitempty" jsonschema:"page size, max 50"`
}

type SearchDatasetsResult struct {
	Total    int                    `json:"total"`
	Datasets []types.DatasetSummary `json:"datasets"`
	// Relaxed is true when the strict term-group search found nothing and the
	// result comes from the OR-combined fallback (lower precision, ranked by
	// relevance). Agents should mention this when presenting results.
	Relaxed bool `json:"relaxed,omitempty"`
}

func summarize(ds *types.Dataset) types.DatasetSummary {
	// ds.Path is "<namespace>/<name>"; the Namespace field is a struct, so the
	// string form is derived from the path.
	namespace := ""
	if idx := strings.LastIndex(ds.Path, "/"); idx > 0 {
		namespace = ds.Path[:idx]
	}
	s := types.DatasetSummary{
		Namespace:   namespace,
		Name:        ds.Name,
		Nickname:    ds.Nickname,
		Description: ds.Description,
		Downloads:   ds.Downloads,
		Likes:       ds.Likes,
	}
	for _, t := range ds.Tags {
		s.Tags = append(s.Tags, t.Name)
	}
	return s
}

func (e *ToolExecutor) SearchDatasets(ctx context.Context, args SearchDatasetsArgs) (*SearchDatasetsResult, error) {
	kwGroups := buildKeywordGroups(args.Keywords)
	var auxGroups [][]string
	if m := strings.ToLower(strings.TrimSpace(args.Modality)); m != "" {
		auxGroups = append(auxGroups, expandToken(m))
	}
	if lang := strings.ToLower(strings.TrimSpace(args.Language)); lang != "" {
		auxGroups = append(auxGroups, expandToken(lang))
	}

	newFilter := func(groups [][]string, any bool, dropTags, tagsOnly bool) *types.RepoFilter {
		// Search mirrors the raw query text: it keeps the store layer on the
		// full-text branch (which requires a non-empty Search) and
		// participates in the cache key alongside the groups. Tags-only
		// attempts must keep Search empty so the store takes the plain
		// tag-join path instead of a full-text match on the raw keywords.
		search := strings.ToLower(strings.TrimSpace(args.Keywords))
		if tagsOnly {
			search = ""
			groups = nil
		} else if search == "" && len(groups) > 0 {
			search = strings.ToLower(strings.TrimSpace(args.Modality + " " + args.Language))
		}
		filter := &types.RepoFilter{
			Search:       search,
			SearchGroups: groups,
			SearchAny:    any,
			RepoSizeMin:  args.MinSizeBytes,
			RepoSizeMax:  args.MaxSizeBytes,
			Sort:         args.Sort,
		}
		if !dropTags {
			for _, t := range args.Tags {
				filter.Tags = append(filter.Tags, types.TagReq{
					Category: t.Category,
					Name:     t.Name,
					Group:    t.Group,
				})
			}
		}
		return filter
	}

	per := args.Per
	if per <= 0 {
		per = defaultPerPage
	}
	if per > maxPerPage {
		per = maxPerPage
	}
	page := args.Page
	if page <= 0 {
		page = 1
	}
	// A zero/negative size bound is not a real filter (LLM clients often emit
	// explicit 0 for "no constraint"); <= 0 max size would match nothing.
	if args.MinSizeBytes != nil && *args.MinSizeBytes <= 0 {
		args.MinSizeBytes = nil
	}
	if args.MaxSizeBytes != nil && *args.MaxSizeBytes <= 0 {
		args.MaxSizeBytes = nil
	}

	allGroups := append(append([][]string{}, kwGroups...), auxGroups...)

	// Precision-to-recall cascade:
	//  1. strict AND over keyword + modality/language groups
	//  2. strict AND over keyword groups only (auxiliary groups dropped)
	//  3. strict AND over auxiliary groups only (keywords dropped) — keywords
	//     are the least reliable channel when the LLM also produced exact tag
	//     filters from the real vocabulary
	//  4. tags only — exact tag joins with no text search at all
	//  5. relaxed OR over all groups, ranked by relevance
	//  6. relaxed OR without tag filters — tags are model-guessed and many
	//     datasets carry no tags at all, so they must not be a hard cutoff
	type attempt struct {
		groups   [][]string
		any      bool
		relaxed  bool
		dropTags bool
		tagsOnly bool
	}
	var attempts []attempt
	if len(allGroups) > 0 {
		attempts = append(attempts, attempt{groups: allGroups})
		if len(auxGroups) > 0 && len(kwGroups) > 0 {
			attempts = append(attempts, attempt{groups: kwGroups})
			if len(args.Tags) > 0 {
				attempts = append(attempts, attempt{groups: auxGroups})
			}
		}
		if len(args.Tags) > 0 {
			attempts = append(attempts, attempt{tagsOnly: true})
		}
		attempts = append(attempts, attempt{groups: allGroups, any: true, relaxed: true})
	} else if len(args.Tags) > 0 {
		attempts = append(attempts, attempt{tagsOnly: true})
	} else {
		attempts = append(attempts, attempt{})
	}
	if len(args.Tags) > 0 && len(allGroups) > 0 {
		attempts = append(attempts, attempt{groups: allGroups, any: true, relaxed: true, dropTags: true})
	}

	for i, att := range attempts {
		filter := newFilter(att.groups, att.any, att.dropTags, att.tagsOnly)
		datasets, total, err := e.datasets.Index(ctx, filter, per, page, false)
		if err != nil {
			return nil, fmt.Errorf("search datasets: %w", err)
		}
		if total == 0 && i < len(attempts)-1 {
			continue
		}
		result := &SearchDatasetsResult{Total: total, Relaxed: att.relaxed && total > 0}
		for _, ds := range datasets {
			result.Datasets = append(result.Datasets, summarize(ds))
		}
		return result, nil
	}
	return &SearchDatasetsResult{}, nil
}

// ---------- list_dataset_tags ----------

type ListDatasetTagsArgs struct {
	Category string `json:"category,omitempty" jsonschema:"only return tags of this category, e.g. task, license, framework, industry"`
}

type TagInfo struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Group    string `json:"group,omitempty"`
}

type ListDatasetTagsResult struct {
	Tags []TagInfo `json:"tags"`
}

func (e *ToolExecutor) ListDatasetTags(ctx context.Context, args ListDatasetTagsArgs) (*ListDatasetTagsResult, error) {
	tags, err := e.tags.AllDatasetTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dataset tags: %w", err)
	}
	result := &ListDatasetTagsResult{}
	for _, t := range tags {
		if args.Category != "" && t.Category != args.Category {
			continue
		}
		result.Tags = append(result.Tags, TagInfo{Category: t.Category, Name: t.Name, Group: t.Group})
	}
	return result, nil
}

// ---------- get_dataset_detail ----------

type GetDatasetDetailArgs struct {
	Namespace string `json:"namespace" jsonschema:"dataset namespace (user or organization)"`
	Name      string `json:"name" jsonschema:"dataset name"`
}

type DatasetDetail struct {
	types.DatasetSummary
	ReadmePreview string `json:"readme_preview,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

func (e *ToolExecutor) GetDatasetDetail(ctx context.Context, args GetDatasetDetailArgs) (*DatasetDetail, error) {
	if args.Namespace == "" || args.Name == "" {
		return nil, fmt.Errorf("namespace and name are required")
	}
	ds, err := e.datasets.Show(ctx, args.Namespace, args.Name, "", false, false)
	if err != nil {
		return nil, fmt.Errorf("get dataset %s/%s: %w", args.Namespace, args.Name, err)
	}
	detail := &DatasetDetail{
		DatasetSummary: summarize(ds),
		CreatedAt:      ds.CreatedAt.Format(time.DateOnly),
	}
	readme, err := e.repo.FileRaw(ctx, &types.GetFileReq{
		Namespace: args.Namespace,
		Name:      args.Name,
		Path:      types.ReadmeFileName,
		Ref:       types.MainBranch,
		RepoType:  types.DatasetRepo,
		Limit:     readmePreviewLimit,
	})
	if err != nil {
		// readme is best-effort; many repos simply lack one
		slog.Debug("get dataset readme failed", slog.String("dataset", args.Namespace+"/"+args.Name), slog.Any("error", err))
	} else {
		detail.ReadmePreview = readme
	}
	return detail, nil
}

// ---------- parse_dataset_query ----------

type ParseDatasetQueryArgs struct {
	Query string `json:"query" jsonschema:"natural language dataset search query"`
}

// ParseDatasetQueryResult mirrors SearchDatasetsArgs so callers can feed the
// parsed output straight into search_datasets.
type ParseDatasetQueryResult struct {
	// Intent is the parsed user intent: "search" (default, possibly with
	// empty filters for browse/list-all), "browse" (list datasets without
	// criteria), "tags" (the user asks which tags/categories exist) or
	// "other" (off-topic, not about datasets). Empty means search.
	Intent       string   `json:"intent,omitempty"`
	Keywords     string   `json:"keywords"`
	Tags         []TagArg `json:"tags,omitempty"`
	Modality     string   `json:"modality,omitempty"`
	Language     string   `json:"language,omitempty"`
	MinSizeBytes *int64   `json:"min_size_bytes,omitempty"`
	MaxSizeBytes *int64   `json:"max_size_bytes,omitempty"`
	// Fallback is true when the LLM was unavailable or its output could not be
	// parsed; Keywords then holds the raw query for plain keyword search.
	Fallback bool   `json:"fallback"`
	Reason   string `json:"reason,omitempty"`
}

const parseQueryPrompt = `You convert natural-language dataset search queries into structured filters for the CSGHub dataset search tool.
Respond with a JSON object only, no markdown fences, no explanation. Schema:
{"intent": "search | browse | tags | other",
 "keywords": "space separated english keywords matched against dataset name and description",
 "tags": [{"category": "task|license|framework|industry|language", "name": "exact tag name"}],
 "modality": "image|audio|video|text|3d (optional, omit if unsure)",
 "language": "chinese|english|... (optional, the natural language of dataset content)",
 "min_size_bytes": "number, omit unless the user specifies a size", "max_size_bytes": "number, omit unless the user specifies a size",
 "reason": "one short sentence describing the user's search intent only; do NOT describe or assume the dataset's content"}
Intent rules: use "tags" when the user asks which tags or tag categories exist (e.g. "有哪些标签"); use "browse" ONLY when the user wants to list or browse datasets without any criteria (e.g. "有哪些数据集", "what datasets are available") — keywords and all filters MUST be empty for browse; a message that names any topic, modality or data type (e.g. "视频数据集", "图像分类的数据", "中文数据集有哪些") is always "search" with the corresponding keywords/modality/language filled in; use "other" when the message is not about datasets at all (greetings, small talk, or unrelated questions); otherwise use "search".
%sWhen the query mentions a task type, license, or content language that matches a known tag, emit it as a tag filter with the exact category and name from the vocabulary (文本分类 -> task/text-classification, 目标检测 -> task/object-detection, 中文 -> language/zh, 英文 -> language/en). Prefer the modality field over task tags for pure modality words (语音/音频 -> audio, 图像/图片 -> image, 视频 -> video, 文本 -> text).
Do not repeat in keywords what you already expressed via tags, modality, language or size filters; keywords are only for the remaining free-text concepts. Omit fields you cannot infer.`

// parseTagVocab formats the real dataset tag vocabulary for the parse prompt
// so the LLM can emit exact tag filters instead of guessing tag names. Only
// task/license/language categories are included; names are capped per
// category to keep the prompt small.
func parseTagVocab(tags []*database.Tag) string {
	const maxNamesPerCategory = 40
	byCategory := make(map[string][]string)
	var order []string
	for _, t := range tags {
		if t.Category != "task" && t.Category != "license" && t.Category != "language" {
			continue
		}
		if _, ok := byCategory[t.Category]; !ok {
			order = append(order, t.Category)
		}
		if len(byCategory[t.Category]) < maxNamesPerCategory {
			byCategory[t.Category] = append(byCategory[t.Category], t.Name)
		}
	}
	if len(order) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Known dataset tags you may use in the tags field (exact category: name pairs): ")
	for i, cat := range order {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(cat)
		b.WriteString(": ")
		b.WriteString(strings.Join(byCategory[cat], ", "))
	}
	b.WriteString(".\n")
	return b.String()
}

func (e *ToolExecutor) ParseDatasetQuery(ctx context.Context, args ParseDatasetQueryArgs) (*ParseDatasetQueryResult, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	endpoint, model, apiKey := e.ResolveLLMConfig(ctx)
	if endpoint == "" {
		return &ParseDatasetQueryResult{Keywords: query, Fallback: true, Reason: "LLM not configured"}, nil
	}

	timeout := time.Duration(e.cfg.AISearch.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	llmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	var tagVocab string
	if e.tags != nil {
		if allTags, err := e.tags.AllDatasetTags(ctx); err == nil {
			tagVocab = parseTagVocab(allTags)
		}
	}
	content, err := e.llm.Chat(llmCtx, endpoint, "", headers, types.LLMReqBody{
		Model: model,
		Messages: []types.LLMMessage{
			{Role: "system", Content: fmt.Sprintf(parseQueryPrompt, tagVocab)},
			{Role: "user", Content: query},
		},
	})
	if err != nil {
		slog.Warn("parse dataset query via llm failed, fallback to keyword search", slog.Any("error", err))
		return &ParseDatasetQueryResult{Keywords: query, Fallback: true, Reason: "LLM request failed"}, nil
	}

	parsed := &ParseDatasetQueryResult{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Trim(content, "`"))), parsed); err != nil {
		slog.Warn("parse llm structured output failed, fallback to keyword search", slog.Any("error", err))
		return &ParseDatasetQueryResult{Keywords: query, Fallback: true, Reason: "LLM output was not valid JSON"}, nil
	}
	// LLMs often emit explicit 0 for the size fields (the schema example shows
	// numbers); a zero bound is not a real constraint and a zero max would
	// match nothing.
	if parsed.MinSizeBytes != nil && *parsed.MinSizeBytes <= 0 {
		parsed.MinSizeBytes = nil
	}
	if parsed.MaxSizeBytes != nil && *parsed.MaxSizeBytes <= 0 {
		parsed.MaxSizeBytes = nil
	}
	// Empty keywords with no filters is a valid browse/list-all intent; only
	// the fallback paths above substitute the raw query as keywords.
	return parsed, nil
}

// ---------- dispatch shared by MCP server and chat agent loop ----------

// Execute runs the named tool with JSON-encoded arguments and returns the
// JSON-encoded result. Used by the chat agent loop; the MCP server registers
// typed handlers instead.
func (e *ToolExecutor) Execute(ctx context.Context, name string, arguments string) (string, error) {
	var (
		result any
		err    error
	)
	switch name {
	case "search_datasets":
		var args SearchDatasetsArgs
		if err = json.Unmarshal([]byte(arguments), &args); err == nil {
			result, err = e.SearchDatasets(ctx, args)
		}
	case "list_dataset_tags":
		var args ListDatasetTagsArgs
		if err = json.Unmarshal([]byte(arguments), &args); err == nil {
			result, err = e.ListDatasetTags(ctx, args)
		}
	case "get_dataset_detail":
		var args GetDatasetDetailArgs
		if err = json.Unmarshal([]byte(arguments), &args); err == nil {
			result, err = e.GetDatasetDetail(ctx, args)
		}
	case "parse_dataset_query":
		var args ParseDatasetQueryArgs
		if err = json.Unmarshal([]byte(arguments), &args); err == nil {
			result, err = e.ParseDatasetQuery(ctx, args)
		}
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal tool %s result: %w", name, err)
	}
	return string(data), nil
}
