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
	"slices"
	"strconv"
	"strings"
	"time"

	"opencsg.com/csghub-server/builder/dataviewer"
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

// RepoSizeLister loads per-branch repository statistics; implemented by
// database.RepoStatisticsStore.
type RepoSizeLister interface {
	FindByRepositoryIDs(ctx context.Context, repoIDs []int64) ([]*database.RepositoryStatistics, error)
}

// DatasetViewer reads the converted preview (catalog and sample rows) of a
// dataset; implemented by builder/dataviewer.DataviewerClient.
type DatasetViewer interface {
	GetCatalog(ctx context.Context, namespace, name string) (*dataviewer.CatalogResult, error)
	GetRows(ctx context.Context, namespace, name, configName, split string, per int) (*dataviewer.RowsResult, error)
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
	repoStats    RepoSizeLister
	viewer       DatasetViewer
	llm          llm.LLMSvcClient
	cfg          *config.Config
	agentConfigs database.AgentConfigStore
}

func NewToolExecutor(datasets DatasetSearcher, repo RepoFileReader, tags DatasetTagLister, repoStats RepoSizeLister, viewer DatasetViewer, llmClient llm.LLMSvcClient, cfg *config.Config, agentConfigs database.AgentConfigStore) *ToolExecutor {
	return &ToolExecutor{
		datasets:     datasets,
		repo:         repo,
		tags:         tags,
		repoStats:    repoStats,
		viewer:       viewer,
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
var ToolNames = []string{"search_datasets", "list_dataset_tags", "get_dataset_detail", "parse_dataset_query", "search_datasets_by_date"}

// ---------- search_datasets ----------

type TagArg struct {
	Category string `json:"category" jsonschema:"tag category, e.g. task, license, framework, industry, program_language"`
	Name     string `json:"name" jsonschema:"exact tag name; call list_dataset_tags first to discover valid values"`
	Group    string `json:"group,omitempty" jsonschema:"optional tag group"`
	// Hard marks a tag the user stated verbatim (matched deterministically
	// against the tag vocabulary), as opposed to an LLM-inferred guess. When
	// every tag is hard, a tag-only fallback search is allowed even with
	// keywords present — the tag IS the user's own constraint.
	Hard bool `json:"hard,omitempty" jsonschema:"set when the tag comes from the user's own words rather than inference"`
}

type SearchDatasetsArgs struct {
	Keywords          string             `json:"keywords,omitempty" jsonschema:"search terms matched against dataset name and description; space-separated, chinese or english; synonyms are expanded automatically"`
	KeywordExpansions []KeywordExpansion `json:"keyword_expansions,omitempty" jsonschema:"equivalent expressions for keywords (e.g. the english form of a chinese term); OR-combined with the source keyword to widen full-text recall"`
	Tags              []TagArg           `json:"tags,omitempty" jsonschema:"exact tag filters (AND-combined); discover valid values with list_dataset_tags"`
	Modality          string             `json:"modality,omitempty" jsonschema:"data modality filter: image, audio, video, text, 3d or multimodal; expanded to synonyms and matched against name and description"`
	// ModalityHard marks the modality as user-stated verbatim; a hard modality
	// is never relaxed away by the fallback cascade.
	ModalityHard bool   `json:"modality_hard,omitempty" jsonschema:"set when the modality word appeared in the user's query itself"`
	Language     string `json:"language,omitempty" jsonschema:"natural language of the dataset content, e.g. chinese, english, 中文; expanded to synonyms"`
	// LanguageHard marks the language as user-stated verbatim; a hard language
	// is never relaxed away by the fallback cascade.
	LanguageHard  bool   `json:"language_hard,omitempty" jsonschema:"set when the language word appeared in the user's query itself"`
	MinSizeBytes  *int64 `json:"min_size_bytes,omitempty" jsonschema:"minimum repository size in bytes; omit unless the user specifies a size"`
	MaxSizeBytes  *int64 `json:"max_size_bytes,omitempty" jsonschema:"maximum repository size in bytes; omit unless the user specifies a size"`
	CreatedAfter  string `json:"created_after,omitempty" jsonschema:"only datasets created on or after this date, YYYY-MM-DD; omit unless the user constrains the creation date"`
	CreatedBefore string `json:"created_before,omitempty" jsonschema:"only datasets created on or before this date, YYYY-MM-DD; omit unless the user constrains the creation date"`
	UpdatedAfter  string `json:"updated_after,omitempty" jsonschema:"only datasets updated on or after this date, YYYY-MM-DD; omit unless the user constrains the update date"`
	UpdatedBefore string `json:"updated_before,omitempty" jsonschema:"only datasets updated on or before this date, YYYY-MM-DD; omit unless the user constrains the update date"`
	Sort          string `json:"sort,omitempty" jsonschema:"sort order: trending, recently_update, most_download, most_favorite, most_star, largest (biggest repo size first) or smallest"`
	Page          int    `json:"page,omitempty" jsonschema:"page number, starting at 1"`
	Per           int    `json:"per,omitempty" jsonschema:"page size, max 50"`
}

type SearchDatasetsResult struct {
	Total    int                    `json:"total"`
	Datasets []types.DatasetSummary `json:"datasets"`
	// MatchMode is "tags" when the winning attempt matched on tags alone
	// (keywords did not literally hit); callers should label the results so
	// users can tell a tag match from a content match.
	MatchMode string `json:"match_mode,omitempty"`
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
		CreatedAt:   ds.CreatedAt.Format(time.DateOnly),
	}
	for _, t := range ds.Tags {
		s.Tags = append(s.Tags, t.Name)
	}
	return s
}

// searchAttempt is one cascade step. noLang marks a retry with the language
// constraint (structured content_lang filter and language text group)
// dropped; only used for LLM-guessed languages, never user-stated ones.
type searchAttempt struct {
	groups   [][]string
	any      bool
	dropTags bool
	tagsOnly bool
	noLang   bool
}

func (e *ToolExecutor) SearchDatasets(ctx context.Context, args SearchDatasetsArgs) (*SearchDatasetsResult, error) {
	// Chinese/English get a structured content-language filter: English
	// datasets never say "english" in name/description, so full-text matching
	// cannot express the condition. Other languages keep the text fallback.
	contentLang := contentLangCode(args.Language)
	keywords := args.Keywords
	if contentLang != "" {
		// the parser may repeat the language inside keywords ("English text
		// classification" with Language=english); the structured filter
		// already covers it, and a full-text group on the language word
		// itself would match nothing real.
		keywords = stripContentLangTerms(keywords, contentLang)
	}
	if (args.MinSizeBytes != nil && *args.MinSizeBytes > 0) || (args.MaxSizeBytes != nil && *args.MaxSizeBytes > 0) {
		// With an explicit size bound, size words in the keywords describe
		// the bound itself, not dataset content.
		keywords = stripSizeDescriptorTerms(keywords)
	}
	kwGroups := buildKeywordGroups(keywords, args.KeywordExpansions)
	var modalityGroups, langGroups [][]string
	if m := strings.ToLower(strings.TrimSpace(args.Modality)); m != "" {
		modalityGroups = append(modalityGroups, expandToken(m))
	}
	if lang := strings.ToLower(strings.TrimSpace(args.Language)); lang != "" && contentLang == "" {
		langGroups = append(langGroups, expandToken(lang))
	}
	auxGroups := append(slices.Clone(modalityGroups), langGroups...)

	createdAfter, err := parseDateBound(args.CreatedAfter, false)
	if err != nil {
		return nil, err
	}
	createdBefore, err := parseDateBound(args.CreatedBefore, true)
	if err != nil {
		return nil, err
	}
	updatedAfter, err := parseDateBound(args.UpdatedAfter, false)
	if err != nil {
		return nil, err
	}
	updatedBefore, err := parseDateBound(args.UpdatedBefore, true)
	if err != nil {
		return nil, err
	}
	clientSort := clientSideSorts[args.Sort]

	newFilter := func(att searchAttempt) *types.RepoFilter {
		groups, any, dropTags, tagsOnly := att.groups, att.any, att.dropTags, att.tagsOnly
		// noLang retries drop the language constraint entirely, structured
		// content_lang filter and language text group alike.
		contentLang := contentLang
		if att.noLang {
			contentLang = ""
		}
		// Search mirrors the raw query text (language terms stripped when the
		// structured filter covers them): it keeps the store layer on the
		// full-text branch (which requires a non-empty Search) and
		// participates in the cache key alongside the groups. Tags-only
		// attempts must keep Search empty so the store takes the plain
		// tag-join path instead of a full-text match on the raw keywords.
		search := strings.ToLower(strings.TrimSpace(keywords))
		switch {
		case tagsOnly:
			search = ""
			groups = nil
		case len(groups) == 0:
			// The keywords reduced to pure stopwords ("数据集", "dataset"), so
			// no text constraint remains; mirroring the raw words into Search
			// would full-text filter on the stopword itself (小于10kb:
			// imagefolder was excluded because its empty README never says
			// "dataset"). Empty Search takes the plain list path.
			search = ""
		case search == "":
			search = strings.ToLower(strings.TrimSpace(args.Modality))
			if contentLang == "" && !att.noLang {
				search = strings.ToLower(strings.TrimSpace(search + " " + args.Language))
			}
			if search == "" {
				// group-only attempts (e.g. tag-name text top-up) still need
				// a non-empty Search so the store takes the full-text branch
				var terms []string
				for _, g := range groups {
					terms = append(terms, g...)
				}
				search = strings.ToLower(strings.Join(terms, " "))
			}
		}
		filter := &types.RepoFilter{
			Search:        search,
			SearchGroups:  groups,
			SearchAny:     any,
			RepoSizeMin:   args.MinSizeBytes,
			RepoSizeMax:   args.MaxSizeBytes,
			CreatedAfter:  createdAfter,
			CreatedBefore: createdBefore,
			UpdatedAfter:  updatedAfter,
			UpdatedBefore: updatedBefore,
			ContentLang:   contentLang,
			Sort:          args.Sort,
		}
		if !dropTags {
			for _, t := range args.Tags {
				// language tags are guesses that mirrored repos rarely carry;
				// when the structured content-language filter is active it
				// decides language alone, so a bogus "language/en" requirement
				// must not exclude datasets that simply lack the tag.
				if contentLang != "" && strings.EqualFold(t.Category, "language") {
					continue
				}
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

	// buildTier assembles one cascade pass. withLang=false produces the
	// language-free pass: same tiers minus the language text group, with
	// noLang set so newFilter also drops the structured content_lang filter.
	// For LLM-guessed (soft) languages this pass runs FIRST; for hard
	// languages it never runs.
	buildTier := func(withLang bool) []searchAttempt {
		aux := modalityGroups
		if withLang {
			aux = auxGroups
		}
		all := append(append([][]string{}, kwGroups...), aux...)
		var list []searchAttempt
		if len(all) > 0 {
			list = append(list, searchAttempt{groups: all, noLang: !withLang})
			if len(aux) > 0 && len(kwGroups) > 0 {
				// A modality the user typed verbatim is never dropped: the
				// keyword-only attempt is what answered "多模态评估" with pure-text
				// benchmarks when the LLM kept "multimodal" only in the modality
				// field instead of the keywords.
				if !args.ModalityHard {
					list = append(list, searchAttempt{groups: kwGroups, noLang: !withLang})
				}
				if len(args.Tags) > 0 {
					list = append(list, searchAttempt{groups: aux, noLang: !withLang})
				}
			}
		}
		if len(args.Tags) > 0 && len(kwGroups) > 0 {
			list = append(list, searchAttempt{groups: kwGroups, dropTags: true, noLang: !withLang})
		}
		// A tags-only attempt drops the keyword constraint entirely, so it is
		// legitimate only when there is nothing to drop (keywords empty or fully
		// consumed by stopwords) or when every tag is user-stated (Hard) — an
		// LLM-guessed tag must never replace the user's own keywords, but a tag
		// the user literally asked for may: keyword search cannot reach datasets
		// whose name/description/README is empty even though they carry the exact
		// tag (图像分类 -> hf-internal-testing-imagefolder..., which has neither).
		if len(args.Tags) > 0 && (len(kwGroups) == 0 || allTagsHard(args.Tags)) {
			list = append(list, searchAttempt{tagsOnly: true, noLang: !withLang})
			// Tag coverage is incomplete on the platform (e.g. HF task_ids never
			// became tags), so an exact tag join under-recalls: a full-text
			// search on the tag names themselves catches datasets whose README
			// describes the task without carrying the tag.
			var tagGroups [][]string
			for _, t := range args.Tags {
				if name := strings.TrimSpace(t.Name); name != "" {
					tagGroups = append(tagGroups, []string{name})
				}
			}
			if len(tagGroups) > 0 {
				list = append(list, searchAttempt{groups: tagGroups, dropTags: true, noLang: !withLang})
			}
		}
		return list
	}

	// Precision cascade with strict AND semantics only. The user's criteria
	// are AND-connected by intent; when a combination does not exist the
	// answer must say so instead of silently dropping a core condition
	// (an earlier OR relaxation answered "多模态评估" with pure-text
	// benchmarks). The only droppable constraints between attempts are the
	// LLM-inferred ones (modality/language groups, guessed tags):
	//  1. strict AND over keyword + modality/language groups (+ tag filters)
	//  2. strict AND over keyword groups only (auxiliary groups dropped) —
	//     skipped when the modality is user-stated (ModalityHard)
	//  3. strict AND over auxiliary groups only (keywords dropped) — keywords
	//     are the least reliable channel when the LLM also produced exact tag
	//     filters from the real vocabulary
	//  4. strict AND over keyword groups without the tag filters — LLM-guessed
	//     tags have incomplete coverage, so a correct tag can still cut away
	//     genuine keyword matches (多选题: only one dataset carries the
	//     multiple-choice tag while several READMEs describe the task)
	//  5. tags only — exact tag joins with no text search at all; only when
	//     there are no content keywords, or every tag is user-stated (Hard)
	// An LLM-guessed language is the softest constraint of all: the model
	// almost always derives it from the query's own language, not from any
	// user intent, and applying it as a filter silently drops matching
	// datasets whenever at least one result survives (小于10kb: guessing
	// "chinese" cut the 2.4–4.7 KB English-README datasets from the answer).
	// So a soft language never filters the primary pass — the cascade runs
	// WITHOUT it first and only retries with it when nothing matched. A
	// language the user typed (LanguageHard) always filters.
	langSoft := args.Language != "" && !args.LanguageHard && (contentLang != "" || len(langGroups) > 0)
	var attempts []searchAttempt
	if langSoft {
		attempts = buildTier(false)
		attempts = append(attempts, buildTier(true)...)
	} else {
		attempts = buildTier(true)
	}
	if len(attempts) == 0 {
		attempts = append(attempts, searchAttempt{noLang: langSoft})
		if langSoft {
			attempts = append(attempts, searchAttempt{})
		}
	}

	// Deterministic orderings are also applied client-side: the full-text
	// search path ranks by relevance and ignores SQL ORDER BY, and an
	// under-filled page merges batches from several queries whose internal
	// ordering does not survive concatenation. A single wider window is
	// fetched and re-sorted by the requested field before paging.
	queryPer, queryPage := per, page
	if clientSort {
		queryPer, queryPage = maxPerPage, 1
	}
	for i, att := range attempts {
		filter := newFilter(att)
		datasets, total, err := e.datasets.Index(ctx, filter, queryPer, queryPage, false)
		if err != nil {
			return nil, fmt.Errorf("search datasets: %w", err)
		}
		slog.Debug("ai search attempt", slog.Int("seq", i), slog.Any("groups", att.groups),
			slog.Bool("drop_tags", att.dropTags), slog.Bool("tags_only", att.tagsOnly), slog.Bool("no_lang", att.noLang),
			slog.Any("tags", filter.Tags), slog.String("content_lang", filter.ContentLang), slog.String("search", filter.Search),
			slog.Int("total", total))
		if total == 0 && i < len(attempts)-1 {
			continue
		}
		// Any text attempt carrying tag filters ANDs them with the groups; an
		// under-filled page means the AND cut away genuine matches (only a
		// minority of datasets carry the tag even when their README describes
		// the task — 多选题: ceval-exam carries multiple-choice while mmlu,
		// ai2_arc and commonsense_qa only mention it in the README). Top up
		// from the two single-constraint supersets — the same text groups
		// without tag filters, then the tags-only join — tag-backed hits
		// first. This is not "relaxed": every returned dataset still satisfies
		// at least one strict constraint. Applies at every cascade position,
		// not just the strict first attempt: a language-relaxed retry tier
		// hits the same tag-coverage wall.
		if len(args.Tags) > 0 && len(att.groups) > 0 && !att.dropTags && !att.tagsOnly && total > 0 && total < per {
			var err error
			datasets, total, err = e.topUpUnderfilled(ctx, newFilter, att, datasets, total, per, queryPer, queryPage)
			if err != nil {
				return nil, err
			}
		}
		// An under-filled tags-only page suffers the same incomplete tag
		// coverage: fill it from the tag-name text search (the next attempt)
		// instead of reporting only the few tag carriers.
		if att.tagsOnly && total > 0 && total < per && i < len(attempts)-1 {
			next := attempts[i+1]
			if !next.tagsOnly {
				extra, extraTotal, err := e.datasets.Index(ctx, newFilter(searchAttempt{groups: next.groups, any: next.any, dropTags: next.dropTags, noLang: next.noLang}), queryPer, queryPage, false)
				if err != nil {
					return nil, fmt.Errorf("top up tags-only search datasets: %w", err)
				}
				total = max(total, extraTotal)
				seen := make(map[string]bool, len(datasets))
				for _, ds := range datasets {
					seen[ds.Path] = true
				}
				for _, ds := range extra {
					if !seen[ds.Path] {
						seen[ds.Path] = true
						datasets = append(datasets, ds)
					}
				}
				total = max(total, len(datasets))
			}
		}
		result := &SearchDatasetsResult{Total: total}
		if att.tagsOnly {
			result.MatchMode = "tags"
		}
		for _, ds := range datasets {
			result.Datasets = append(result.Datasets, summarize(ds))
		}
		e.fillSizes(ctx, datasets, result.Datasets)
		if clientSort {
			sortMergedDatasets(datasets, result.Datasets, args.Sort)
			result.Datasets = slicePage(result.Datasets, per, page)
		}
		return result, nil
	}
	return &SearchDatasetsResult{}, nil
}

// ---------- search_datasets_by_date ----------

// SearchDatasetsByDateArgs is the explicit date-window search: dates arrive
// as plain YYYY-MM-DD and are applied exactly, with no natural-language date
// interpretation anywhere on the path.
type SearchDatasetsByDateArgs struct {
	DateField string `json:"date_field" jsonschema:"which date to filter on: created (creation/upload) or updated (last update)"`
	After     string `json:"after,omitempty" jsonschema:"only datasets whose date is on or after this day, YYYY-MM-DD"`
	Before    string `json:"before,omitempty" jsonschema:"only datasets whose date is on or before this day, YYYY-MM-DD"`
	Keywords  string `json:"keywords,omitempty" jsonschema:"optional extra search terms matched against dataset name and description"`
	Page      int    `json:"page,omitempty" jsonschema:"page number, starting at 1"`
	Per       int    `json:"per,omitempty" jsonschema:"page size, max 50"`
}

// SearchDatasetsByDate is a thin deterministic wrapper over SearchDatasets:
// it only maps the date window onto the created_at/updated_at bounds.
func (e *ToolExecutor) SearchDatasetsByDate(ctx context.Context, args SearchDatasetsByDateArgs) (*SearchDatasetsResult, error) {
	field := strings.ToLower(strings.TrimSpace(args.DateField))
	if field != "created" && field != "updated" {
		return nil, fmt.Errorf("date_field must be \"created\" or \"updated\"")
	}
	if strings.TrimSpace(args.After) == "" && strings.TrimSpace(args.Before) == "" {
		return nil, fmt.Errorf("after or before is required")
	}
	searchArgs := SearchDatasetsArgs{
		Keywords: args.Keywords,
		Page:     args.Page,
		Per:      args.Per,
	}
	if field == "created" {
		searchArgs.CreatedAfter, searchArgs.CreatedBefore = args.After, args.Before
	} else {
		searchArgs.UpdatedAfter, searchArgs.UpdatedBefore = args.After, args.Before
	}
	return e.SearchDatasets(ctx, searchArgs)
}

// parseDateBound parses a YYYY-MM-DD (or RFC3339) date bound; endOfDay makes
// a date-only upper bound inclusive of that whole day.
func parseDateBound(value string, endOfDay bool) (*time.Time, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("invalid date %q, want YYYY-MM-DD", value)
}

// fillSizes populates the Size field of each summary from the repository
// statistics of its default branch. Best-effort: failures leave sizes at 0.
func (e *ToolExecutor) fillSizes(ctx context.Context, datasets []*types.Dataset, summaries []types.DatasetSummary) {
	if e.repoStats == nil || len(datasets) == 0 {
		return
	}
	type statsKey struct {
		repoID int64
		branch string
	}
	repoIDs := make([]int64, 0, len(datasets))
	for _, ds := range datasets {
		repoIDs = append(repoIDs, ds.RepositoryID)
	}
	stats, err := e.repoStats.FindByRepositoryIDs(ctx, repoIDs)
	if err != nil {
		slog.Warn("fill dataset sizes failed", slog.Any("error", err))
		return
	}
	sizeByKey := make(map[statsKey]int64, len(stats))
	for _, st := range stats {
		sizeByKey[statsKey{st.RepositoryID, st.Branch}] = st.TotalSize
	}
	for i, ds := range datasets {
		summaries[i].Size = sizeByKey[statsKey{ds.RepositoryID, ds.DefaultBranch}]
	}
}

// clientSideSorts are the deterministic orderings re-sorted in memory after
// the cascade: the full-text path ranks by relevance (ignoring SQL ORDER BY)
// and under-filled pages merge batches from several queries, so only a
// client-side pass guarantees the requested global order. trending and
// most_star stay SQL-side — the recom score and the repository star count
// are not loaded onto types.Dataset here.
var clientSideSorts = map[string]bool{
	"largest":         true,
	"smallest":        true,
	"most_download":   true,
	"most_favorite":   true,
	"recently_update": true,
}

// sortMergedDatasets re-sorts the merged result window by the requested
// deterministic ordering. datasets and summaries are parallel slices (one
// summary per dataset, sizes already filled); both are reordered together.
// The sort is stable, so ties keep the cascade order (strict matches first).
func sortMergedDatasets(datasets []*types.Dataset, summaries []types.DatasetSummary, sort string) {
	less := func(i, j int) bool {
		a, b := datasets[i], datasets[j]
		switch sort {
		case "smallest":
			return summaries[i].Size < summaries[j].Size
		case "largest":
			return summaries[i].Size > summaries[j].Size
		case "most_download":
			return a.Downloads > b.Downloads
		case "most_favorite":
			return a.Likes > b.Likes
		case "recently_update":
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return false
	}
	perm := make([]int, len(datasets))
	for i := range perm {
		perm[i] = i
	}
	slices.SortStableFunc(perm, func(x, y int) int {
		switch {
		case less(x, y):
			return -1
		case less(y, x):
			return 1
		default:
			return 0
		}
	})
	sortedDs := make([]*types.Dataset, len(datasets))
	sortedSummaries := make([]types.DatasetSummary, len(summaries))
	for to, from := range perm {
		sortedDs[to] = datasets[from]
		sortedSummaries[to] = summaries[from]
	}
	copy(datasets, sortedDs)
	copy(summaries, sortedSummaries)
}

// topUpUnderfilled supplements an under-filled strict text+tag page from the
// two single-constraint supersets: first the same strict text search without
// tag filters, then the tags-only join. Existing hits keep their order; new
// hits append. The total is the largest superset count observed, so the
// answer never reports fewer matches than a single-constraint query found.
func (e *ToolExecutor) topUpUnderfilled(ctx context.Context, newFilter func(att searchAttempt) *types.RepoFilter, won searchAttempt, datasets []*types.Dataset, total, per, queryPer, queryPage int) ([]*types.Dataset, int, error) {
	seen := make(map[string]bool, len(datasets))
	for _, ds := range datasets {
		seen[ds.Path] = true
	}
	topUps := []*types.RepoFilter{
		newFilter(searchAttempt{groups: won.groups, dropTags: true, noLang: won.noLang}),
		newFilter(searchAttempt{tagsOnly: true, noLang: won.noLang}),
	}
	for _, filter := range topUps {
		if len(datasets) >= per {
			break
		}
		extra, extraTotal, err := e.datasets.Index(ctx, filter, queryPer, queryPage, false)
		if err != nil {
			return nil, 0, fmt.Errorf("top up search datasets: %w", err)
		}
		total = max(total, extraTotal)
		for _, ds := range extra {
			if !seen[ds.Path] {
				seen[ds.Path] = true
				datasets = append(datasets, ds)
			}
		}
	}
	total = max(total, len(datasets))
	return datasets, total, nil
}

// slicePage extracts the requested page from a wider pre-fetched window.
func slicePage(datasets []types.DatasetSummary, per, page int) []types.DatasetSummary {
	begin := (page - 1) * per
	if begin >= len(datasets) {
		return nil
	}
	end := min(begin+per, len(datasets))
	return datasets[begin:end]
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
	License       string   `json:"license,omitempty"`
	Tasks         []string `json:"tasks,omitempty"`
	ReadmePreview string   `json:"readme_preview,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	// Splits, RowsNum, SampleColumns and SampleRows come from the dataviewer
	// preview conversion; they are only present when the dataset has been
	// converted for preview.
	Splits        []SplitSummary `json:"splits,omitempty"`
	RowsNum       int64          `json:"rows_num,omitempty"`
	SampleColumns []string       `json:"sample_columns,omitempty"`
	SampleRows    [][]any        `json:"sample_rows,omitempty"`
}

// SplitSummary is one converted split of a dataset config.
type SplitSummary struct {
	Config string `json:"config"`
	Name   string `json:"name"`
	Rows   int    `json:"rows"`
}

// sampleRowCount caps how many sample rows get_dataset_detail embeds.
const sampleRowCount = 3

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
		License:        ds.License,
		CreatedAt:      ds.CreatedAt.Format(time.DateOnly),
	}
	for _, t := range ds.Tags {
		if t.Category == "task" {
			detail.Tasks = append(detail.Tasks, t.Name)
		}
	}
	summaries := []types.DatasetSummary{detail.DatasetSummary}
	e.fillSizes(ctx, []*types.Dataset{ds}, summaries)
	detail.DatasetSummary = summaries[0]
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
	e.fillViewerInfo(ctx, args.Namespace, args.Name, detail)
	return detail, nil
}

// fillViewerInfo adds the converted preview structure (splits, row count) and
// a few sample rows to the detail. Best-effort: a missing or unconverted
// preview leaves the fields empty.
func (e *ToolExecutor) fillViewerInfo(ctx context.Context, namespace, name string, detail *DatasetDetail) {
	if e.viewer == nil {
		return
	}
	catalog, err := e.viewer.GetCatalog(ctx, namespace, name)
	if err != nil {
		slog.Debug("get dataset catalog failed", slog.String("dataset", namespace+"/"+name), slog.Any("error", err))
		return
	}
	detail.RowsNum = catalog.RowsNum
	for _, info := range catalog.DatasetInfos {
		for _, sp := range info.Splits {
			detail.Splits = append(detail.Splits, SplitSummary{Config: info.ConfigName, Name: sp.Name, Rows: sp.NumExamples})
		}
	}
	if len(catalog.DatasetInfos) == 0 || len(catalog.DatasetInfos[0].Splits) == 0 {
		return
	}
	first := catalog.DatasetInfos[0]
	rows, err := e.viewer.GetRows(ctx, namespace, name, first.ConfigName, first.Splits[0].Name, sampleRowCount)
	if err != nil {
		slog.Debug("get dataset sample rows failed", slog.String("dataset", namespace+"/"+name), slog.Any("error", err))
		return
	}
	detail.SampleColumns = rows.Columns
	detail.SampleRows = rows.Rows
}

// ---------- parse_dataset_query ----------

type ParseDatasetQueryArgs struct {
	Query string `json:"query" jsonschema:"natural language dataset search query"`
	// RawQuery is the user's current message before any conversation history
	// was folded into Query. Deterministic constraint extraction (dates,
	// explicit modality/language/tags) runs against it when present, so
	// earlier turns cannot pin constraints the user has already moved away
	// from (e.g. "那视频的呢" after an image search).
	RawQuery string `json:"raw_query,omitempty" jsonschema:"the user's current message, when query contains folded conversation history"`
	// History holds the earlier user messages (oldest first) when Query has
	// conversation context folded in. A follow-up that adds no criteria of its
	// own ("最大的数据集是谁") inherits the constraints those messages stated.
	History []string `json:"history,omitempty" jsonschema:"earlier user messages, oldest first, when query contains folded conversation history"`
}

// ParseDatasetQueryResult mirrors SearchDatasetsArgs so callers can feed the
// parsed output straight into search_datasets.
type ParseDatasetQueryResult struct {
	// Intent is the parsed user intent: "search" (default, possibly with
	// empty filters for browse/list-all), "browse" (list datasets without
	// criteria), "tags" (the user asks which tags/categories exist), "detail"
	// (the user asks for the basic info of one specific dataset) or "other"
	// (off-topic, not about datasets). Empty means search.
	Intent string `json:"intent,omitempty"`
	// Dataset names one specific dataset (namespace/name or a bare name) when
	// intent is "detail".
	Dataset  string `json:"dataset,omitempty"`
	Keywords string `json:"keywords"`
	// KeywordExpansions carries LLM-produced equivalent expressions for the
	// keywords (always including the english form), widened into the search
	// groups so Chinese queries can hit English README/description content.
	KeywordExpansions []KeywordExpansion `json:"keyword_expansions,omitempty"`
	Tags              []TagArg           `json:"tags,omitempty"`
	Modality          string             `json:"modality,omitempty"`
	// ModalityHard is set when the modality word appeared verbatim in the
	// user's message (deterministic extraction), not merely inferred by the
	// LLM; hard modalities survive the search fallback cascade.
	ModalityHard bool   `json:"modality_hard,omitempty"`
	Language     string `json:"language,omitempty"`
	// LanguageHard is set when the language word appeared verbatim in the
	// user's message; hard languages survive the search fallback cascade.
	LanguageHard bool   `json:"language_hard,omitempty"`
	MinSizeBytes *int64 `json:"min_size_bytes,omitempty"`
	MaxSizeBytes *int64 `json:"max_size_bytes,omitempty"`
	// CreatedAfter/CreatedBefore bound the dataset creation date (YYYY-MM-DD).
	CreatedAfter  string `json:"created_after,omitempty"`
	CreatedBefore string `json:"created_before,omitempty"`
	// UpdatedAfter/UpdatedBefore bound the dataset update date (YYYY-MM-DD).
	UpdatedAfter  string `json:"updated_after,omitempty"`
	UpdatedBefore string `json:"updated_before,omitempty"`
	// Sort is the native list ordering (trending, recently_update,
	// most_download, most_favorite, most_star, largest, smallest); only set
	// when the user asks for an ordering such as "下载最多" or "最大的数据集".
	Sort string `json:"sort,omitempty"`
	// Fallback is true when the LLM was unavailable or its output could not be
	// parsed; Keywords then holds the raw query for plain keyword search.
	Fallback bool   `json:"fallback"`
	Reason   string `json:"reason,omitempty"`
}

// UnmarshalJSON tolerates LLM-issued string values in the integer size
// fields ("" or "1048576" instead of a number) instead of rejecting the
// whole parse result — different models format these fields differently.
func (r *ParseDatasetQueryResult) UnmarshalJSON(data []byte) error {
	type plain ParseDatasetQueryResult
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{"min_size_bytes", "max_size_bytes"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue // already a number or null, standard decode handles it
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			delete(raw, key) // empty or unparsable: treat as absent
			continue
		}
		raw[key] = json.RawMessage(strconv.FormatInt(n, 10))
	}
	fixed, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(fixed, (*plain)(r))
}

const parseQueryPrompt = `You convert natural-language dataset search queries into structured filters for the CSGHub dataset search tool.
Current date: %s.
Respond with a JSON object only, no markdown fences, no explanation. Schema:
{"intent": "search | browse | tags | detail | other",
 "dataset": "namespace/name or bare dataset name, only for intent=detail",
 "keywords": "space separated english keywords matched against dataset name and description",
 "keyword_expansions": [{"term": "a word from keywords", "equivalents": ["1-5 equivalent expressions, always including the common english form"]}],
 "tags": [{"category": "task|license|framework|industry|language", "name": "exact tag name"}],
 "modality": "image|audio|video|text|3d|multimodal (optional, omit if unsure)",
 "language": "chinese|english|... (optional, the natural language of dataset content)",
 "min_size_bytes": "number, omit unless the user specifies a size", "max_size_bytes": "number, omit unless the user specifies a size",
 "created_after": "YYYY-MM-DD, omit unless the user constrains the creation/upload date", "created_before": "YYYY-MM-DD, omit unless the user constrains the creation/upload date",
 "updated_after": "YYYY-MM-DD, omit unless the user constrains the update date", "updated_before": "YYYY-MM-DD, omit unless the user constrains the update date",
 "sort": "trending|recently_update|most_download|most_favorite|most_star|largest|smallest (optional, only when the user asks for an ordering)",
 "reason": "one short sentence describing the user's search intent only; do NOT describe or assume the dataset's content"}
Intent rules: use "tags" when the user asks which tags or tag categories exist (e.g. "有哪些标签"); use "detail" when the user asks for the basic information of ONE specific named dataset — its purpose, modality, license, size or samples (e.g. "mmlu 这个数据集的基本信息", "介绍一下 ceval-exam", "opencompass/mmlu 的许可是什么", "这个数据集有什么用途" referring to a dataset named earlier) — set the dataset field to the name exactly as the user wrote it: the bare name unless the user explicitly wrote namespace/name, NEVER guess or invent a namespace; leave all filters empty; use "browse" ONLY when the user wants to list or browse datasets without any criteria (e.g. "有哪些数据集", "what datasets are available") — keywords and all filters MUST be empty for browse; a message that names any topic, modality or data type (e.g. "视频数据集", "图像分类的数据", "中文数据集有哪些") is always "search" with the corresponding keywords/modality/language filled in; a date or size constraint is a criterion too, so such queries are "search" (with empty keywords if there is nothing else, e.g. "2025年之后上传的数据集" -> intent=search, created_after=2025-01-01); use "other" when the message is not about datasets at all (greetings, small talk, or unrelated questions); otherwise use "search".
Sort rules: ordering words are NOT search keywords — map them to the sort field instead (下载最多/最多下载/最热门 -> most_download, 收藏最多 -> most_favorite, 星标最多/最多 star -> most_star, 最近更新/最新 -> recently_update, 体积最大/数据量最大/最大的 -> largest, 体积最小/最小的 -> smallest, 热门趋势 -> trending). A query that only asks for an ordering (e.g. "下载最多的数据集有哪些", "most downloaded datasets") is "browse" with sort set; omit sort when the user does not ask for an ordering.
Date rules: convert relative dates (上个月, 去年, 最近三个月) to absolute YYYY-MM-DD using the current date above; an open-ended range sets only one bound (2025年之后 -> created_after=2025-01-01). Distinguish creation from update: use created_after/created_before ONLY for creation/upload phrasing (创建/上传/导入/发布); use updated_after/updated_before for update phrasing (更新/最近更新, e.g. "今天更新的数据集" -> updated_after=<today>); when the user asks for recently updated datasets without a concrete date, set sort=recently_update and leave all date bounds empty.
Size rules: convert sizes to bytes with 1024-based units (10kb -> 10240, 1.5GB -> 1610612736); 小于10kb -> max_size_bytes=10240, 大于2MB -> min_size_bytes=2097152.
Follow-up rules: the user message may start with 对话上下文 (earlier conversation turns) followed by 当前问题 (the question to parse). When 当前问题 refers back to the previous results without stating new filter criteria (e.g. "最大的数据集是谁", "哪个最小", "下载量最高的是谁", "它们有中文的吗"), the user is still asking about those results: keep the earlier search's topic in keywords (normalized as usual) and inherit the filter conditions (size, dates, modality, language, tags) from the earlier user turns; drop an inherited condition only when 当前问题 states a different one. When 当前问题 introduces a new topic, use only its own criteria — never treat history text as new keywords then.
%sWhen the query mentions a task type, license, or content language that matches a known tag, emit it as a tag filter with the exact category and name from the vocabulary (文本分类 -> task/text-classification, 目标检测 -> task/object-detection, 中文 -> language/zh, 英文 -> language/en). Prefer the modality field over task tags for pure modality words (语音/音频 -> audio, 图像/图片 -> image, 视频 -> video, 文本 -> text, 多模态 -> multimodal).
Do not repeat in keywords what you already expressed via modality, language, sort or size filters. Task and license tags, however, have incomplete coverage on this platform (many datasets lack the tag even though their README describes the task), so when you emit a task or license tag, ALSO keep that concept in keywords with expansions — the tag narrows precisely where present, the keywords catch the rest. Keywords are only for the remaining free-text concepts. Omit fields you cannot infer.
Keyword expansion rules: dataset names, descriptions and README docs on this platform are mostly english, so for every content keyword provide 1-5 equivalent or synonymous expressions in keyword_expansions, ALWAYS including the common english form (多选题 -> multiple choice, multiple-choice, mcq; 自动驾驶 -> autonomous driving, self-driving). The term must be a word that appears in keywords; skip keywords with no useful equivalents, and omit the whole field when there is nothing to expand; do not expand words already expressed via modality or language.`

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

// fallbackParseResult builds the raw-keyword fallback used when the LLM is
// unavailable or its output is unusable. Deterministic constraints (dates,
// explicit modality/language/tags) are still honored so an LLM outage does
// not silently drop conditions the user stated outright. Keywords hold the
// current message only — not the history-folded query, which full-text search
// cannot use.
func (e *ToolExecutor) fallbackParseResult(raw, reason string, vocab []*database.Tag) *ParseDatasetQueryResult {
	r := &ParseDatasetQueryResult{Keywords: raw, Fallback: true, Reason: reason}
	applyDateConstraint(r, raw)
	applySizeConstraint(r, raw)
	applyExplicitConstraints(r, raw, vocab)
	return r
}

// formatDateBound renders an optional date bound as YYYY-MM-DD.
func formatDateBound(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.DateOnly)
}

// applyDateConstraint overrides the result's date fields with the
// deterministically parsed date expression, if the query contains one. Dates
// are NOT trusted to the LLM: compact forms like "10.12" are occasionally
// misread (observed as 2026-02-12). Unrecognized fuzzy phrasings keep the
// LLM's interpretation.
func applyDateConstraint(parsed *ParseDatasetQueryResult, query string) {
	dc := extractDateConstraint(query, time.Now())
	if dc == nil {
		return
	}
	parsed.CreatedAfter, parsed.CreatedBefore, parsed.UpdatedAfter, parsed.UpdatedBefore = "", "", "", ""
	if dc.updated {
		parsed.UpdatedAfter, parsed.UpdatedBefore = formatDateBound(dc.after), formatDateBound(dc.before)
	} else {
		parsed.CreatedAfter, parsed.CreatedBefore = formatDateBound(dc.after), formatDateBound(dc.before)
	}
	// an explicit date outranks the vague "recently updated" ordering
	if parsed.Sort == "recently_update" {
		parsed.Sort = ""
	}
}

// inheritHistoryConstraints fills filter fields the current message left
// empty with constraints the user stated in earlier turns, so a follow-up
// like "最大的数据集是谁" stays inside the scope the conversation established
// ("小于10kb的数据集" first). The fill is per-field: an explicit choice in the
// current message always wins ("那视频的呢" overrides an inherited image
// modality). Keywords are inherited too, but only the terms recognized by the
// synonym vocabulary (recognizedTopicTerms) and only when the current message
// carries no search terms of its own — raw history text is too noisy to
// transfer verbatim, and a topic switch must start clean. Tags the user
// stated earlier and the LLM repeated softly are upgraded to Hard. Only
// deterministic extraction is used; constraints the LLM guessed for earlier
// turns are not replayed.
func inheritHistoryConstraints(parsed *ParseDatasetQueryResult, raw string, history []string, vocab []*database.Tag) {
	if len(history) == 0 || parsed.Fallback {
		return
	}
	now := time.Now()
	inheritKeywords := strings.TrimSpace(parsed.Keywords) == "" && !HasSearchTerms(raw)
	for i := len(history) - 1; i >= 0; i-- {
		msg := strings.TrimSpace(history[i])
		if msg == "" {
			continue
		}
		if inheritKeywords {
			if kw := recognizedTopicTerms(msg); kw != "" {
				parsed.Keywords = kw
				inheritKeywords = false
			}
		}
		if parsed.MinSizeBytes == nil && parsed.MaxSizeBytes == nil {
			if sc := extractSizeConstraint(msg); sc != nil {
				parsed.MinSizeBytes, parsed.MaxSizeBytes = sc.min, sc.max
			}
		}
		if parsed.Modality == "" {
			if m := explicitModality(msg); m != "" {
				parsed.Modality, parsed.ModalityHard = m, true
			}
		}
		if parsed.Language == "" {
			if l := explicitLanguage(msg); l != "" {
				parsed.Language, parsed.LanguageHard = l, true
			}
		}
		explicit := matchExplicitTags(msg, vocab)
		if len(parsed.Tags) == 0 {
			parsed.Tags = explicit
		} else {
			// a tag the user typed earlier stays user-stated when the LLM
			// repeats it for the follow-up (LLM copies arrive soft)
			for j := range parsed.Tags {
				for _, et := range explicit {
					if parsed.Tags[j].Category == et.Category && parsed.Tags[j].Name == et.Name {
						parsed.Tags[j].Hard = true
					}
				}
			}
		}
		if parsed.CreatedAfter == "" && parsed.CreatedBefore == "" && parsed.UpdatedAfter == "" && parsed.UpdatedBefore == "" {
			if dc := extractDateConstraint(msg, now); dc != nil {
				if dc.updated {
					parsed.UpdatedAfter, parsed.UpdatedBefore = formatDateBound(dc.after), formatDateBound(dc.before)
				} else {
					parsed.CreatedAfter, parsed.CreatedBefore = formatDateBound(dc.after), formatDateBound(dc.before)
				}
			}
		}
	}
}

func (e *ToolExecutor) ParseDatasetQuery(ctx context.Context, args ParseDatasetQueryArgs) (*ParseDatasetQueryResult, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	// Deterministic extraction (dates, explicit modality/language/tags) runs
	// against the current message only, never the folded history.
	raw := strings.TrimSpace(args.RawQuery)
	if raw == "" {
		raw = query
	}
	var vocab []*database.Tag
	if e.tags != nil {
		if allTags, err := e.tags.AllDatasetTags(ctx); err == nil {
			vocab = allTags
		}
	}
	endpoint, model, apiKey := e.ResolveLLMConfig(ctx)
	if endpoint == "" {
		return e.fallbackParseResult(raw, "LLM not configured", vocab), nil
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
	tagVocab := parseTagVocab(vocab)
	content, err := e.llm.Chat(llmCtx, endpoint, "", headers, types.LLMReqBody{
		Model: model,
		Messages: []types.LLMMessage{
			{Role: "system", Content: fmt.Sprintf(parseQueryPrompt, time.Now().Format(time.DateOnly), tagVocab)},
			{Role: "user", Content: query},
		},
	})
	if err != nil {
		slog.Warn("parse dataset query via llm failed, fallback to keyword search", slog.Any("error", err))
		return e.fallbackParseResult(raw, "LLM request failed", vocab), nil
	}

	parsed := &ParseDatasetQueryResult{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Trim(content, "`"))), parsed); err != nil {
		slog.Warn("parse llm structured output failed, fallback to keyword search", slog.Any("error", err))
		return e.fallbackParseResult(raw, "LLM output was not valid JSON", vocab), nil
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
	// Only the native sort orders are accepted; an unrecognized LLM-emitted
	// value would silently fall back to the store default, so drop it here.
	if !slices.Contains(types.DatasetSorts, parsed.Sort) {
		parsed.Sort = ""
	}
	// Same for date bounds: an unparseable LLM-emitted value would otherwise
	// fail the whole search; dropping it keeps the rest of the criteria.
	if _, err := parseDateBound(parsed.CreatedAfter, false); err != nil {
		parsed.CreatedAfter = ""
	}
	if _, err := parseDateBound(parsed.CreatedBefore, true); err != nil {
		parsed.CreatedBefore = ""
	}
	if _, err := parseDateBound(parsed.UpdatedAfter, false); err != nil {
		parsed.UpdatedAfter = ""
	}
	if _, err := parseDateBound(parsed.UpdatedBefore, true); err != nil {
		parsed.UpdatedBefore = ""
	}
	// Expansions are a best-effort recall aid: normalize them and drop junk
	// entries rather than failing the search on malformed LLM output.
	parsed.KeywordExpansions = sanitizeKeywordExpansions(parsed.KeywordExpansions)
	applyDateConstraint(parsed, raw)
	applySizeConstraint(parsed, raw)
	applyExplicitConstraints(parsed, raw, vocab)
	inheritHistoryConstraints(parsed, raw, args.History, vocab)
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
	case "search_datasets_by_date":
		var args SearchDatasetsByDateArgs
		if err = json.Unmarshal([]byte(arguments), &args); err == nil {
			result, err = e.SearchDatasetsByDate(ctx, args)
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
