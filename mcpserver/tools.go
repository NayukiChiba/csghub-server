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

// modalityKeywords augments keyword search for a requested data modality. Tags
// are the precise filter (AND semantics in the store layer), so modality terms
// are added as keywords instead; agents can combine list_dataset_tags with the
// tags parameter for exact task-tag matching.
var modalityKeywords = map[string]string{
	"image": "image",
	"audio": "audio speech voice sound",
	"video": "video",
	"text":  "text nlp language",
	"3d":    "3d point cloud",
}

// ToolExecutor executes the dataset search tools. It is shared by the MCP
// server and the in-process chat agent loop.
type ToolExecutor struct {
	datasets DatasetSearcher
	repo     RepoFileReader
	tags     DatasetTagLister
	llm      llm.LLMSvcClient
	cfg      *config.Config
}

func NewToolExecutor(datasets DatasetSearcher, repo RepoFileReader, tags DatasetTagLister, llmClient llm.LLMSvcClient, cfg *config.Config) *ToolExecutor {
	return &ToolExecutor{
		datasets: datasets,
		repo:     repo,
		tags:     tags,
		llm:      llmClient,
		cfg:      cfg,
	}
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
	Keywords     string   `json:"keywords,omitempty" jsonschema:"search keywords matched against dataset name and description; multiple words are space-separated"`
	Tags         []TagArg `json:"tags,omitempty" jsonschema:"exact tag filters (AND-combined); discover valid values with list_dataset_tags"`
	Modality     string   `json:"modality,omitempty" jsonschema:"data modality filter: image, audio, video, text or 3d; augments keywords"`
	Language     string   `json:"language,omitempty" jsonschema:"natural language of the dataset content, e.g. chinese, english; augments keywords"`
	MinSizeBytes *int64   `json:"min_size_bytes,omitempty" jsonschema:"minimum repository size in bytes"`
	MaxSizeBytes *int64   `json:"max_size_bytes,omitempty" jsonschema:"maximum repository size in bytes"`
	Sort         string   `json:"sort,omitempty" jsonschema:"sort order: trending, recently_update, most_download, most_favorite or most_star"`
	Page         int      `json:"page,omitempty" jsonschema:"page number, starting at 1"`
	Per          int      `json:"per,omitempty" jsonschema:"page size, max 50"`
}

type SearchDatasetsResult struct {
	Total    int                    `json:"total"`
	Datasets []types.DatasetSummary `json:"datasets"`
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
	keywords := strings.TrimSpace(args.Keywords)
	if kw, ok := modalityKeywords[strings.ToLower(args.Modality)]; ok {
		keywords = strings.TrimSpace(keywords + " " + kw)
	}
	if lang := strings.TrimSpace(args.Language); lang != "" {
		keywords = strings.TrimSpace(keywords + " " + lang)
	}

	filter := &types.RepoFilter{
		Search:      keywords,
		RepoSizeMin: args.MinSizeBytes,
		RepoSizeMax: args.MaxSizeBytes,
		Sort:        args.Sort,
	}
	for _, t := range args.Tags {
		filter.Tags = append(filter.Tags, types.TagReq{
			Category: t.Category,
			Name:     t.Name,
			Group:    t.Group,
		})
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

	datasets, total, err := e.datasets.Index(ctx, filter, per, page, false)
	if err != nil {
		return nil, fmt.Errorf("search datasets: %w", err)
	}
	result := &SearchDatasetsResult{Total: total}
	for _, ds := range datasets {
		result.Datasets = append(result.Datasets, summarize(ds))
	}
	return result, nil
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
{"keywords": "space separated english keywords matched against dataset name and description",
 "tags": [{"category": "task|license|framework|industry", "name": "exact tag name"}],
 "modality": "image|audio|video|text|3d (optional, omit if unsure)",
 "language": "chinese|english|... (optional, the natural language of dataset content)",
 "min_size_bytes": 0, "max_size_bytes": 0,
 "reason": "one short sentence explaining your interpretation"}
Only include tags you are confident exist (common task tags look like image-classification, automatic-speech-recognition, text-generation). Omit fields you cannot infer.`

func (e *ToolExecutor) ParseDatasetQuery(ctx context.Context, args ParseDatasetQueryArgs) (*ParseDatasetQueryResult, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if e.cfg.AISearch.LLMEndpoint == "" {
		return &ParseDatasetQueryResult{Keywords: query, Fallback: true, Reason: "LLM not configured"}, nil
	}

	timeout := time.Duration(e.cfg.AISearch.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	llmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	headers := map[string]string{}
	if e.cfg.AISearch.LLMAPIKey != "" {
		headers["Authorization"] = "Bearer " + e.cfg.AISearch.LLMAPIKey
	}
	content, err := e.llm.Chat(llmCtx, e.cfg.AISearch.LLMEndpoint, "", headers, types.LLMReqBody{
		Model: e.cfg.AISearch.LLMModel,
		Messages: []types.LLMMessage{
			{Role: "system", Content: parseQueryPrompt},
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
	if parsed.Keywords == "" {
		parsed.Keywords = query
	}
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
