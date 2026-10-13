package mcpserver

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// querySynonymGroups lists domain synonyms for dataset search. Terms inside
// one group are treated as equivalents (OR-combined); the agent's keyword
// tokens are expanded through these groups so Chinese and English queries hit
// the same documents without any embedding/RAG infrastructure.
//
// Keep each term a single lowercase token (multi-word entries are allowed;
// they are passed through plainto_tsquery). When adding a term that appears in
// two groups, the later group wins for direct lookup.
var querySynonymGroups = [][]string{
	// modality
	{"image", "images", "img", "picture", "pictures", "photo", "photos", "vision", "cv", "图像", "图片", "照片", "影像", "视觉"},
	{"video", "videos", "视频"},
	{"audio", "speech", "voice", "sound", "asr", "tts", "音频", "语音", "声音"},
	{"text", "texts", "nlp", "corpus", "文本", "语料", "语料库"},
	{"3d", "pointcloud", "point cloud", "三维", "点云"},
	{"multimodal", "multi-modal", "多模态"},

	// task
	{"classification", "classify", "classifier", "分类"},
	{"detection", "detect", "detector", "检测"},
	{"segmentation", "segment", "分割"},
	{"recognition", "recognize", "识别"},
	{"ocr", "光学字符识别"},
	{"translation", "translate", "翻译"},
	{"summarization", "summary", "摘要", "总结"},
	{"qa", "question answering", "问答"},
	{"sentiment", "情感"},
	{"generation", "generate", "生成"},
	{"dialogue", "dialog", "conversation", "chat", "对话", "聊天"},
	{"instruction", "instructions", "sft", "指令"},
	{"embedding", "embeddings", "向量", "嵌入"},
	{"reasoning", "推理"},
	{"object", "objects", "目标", "物体"},
	{"face", "faces", "facial", "人脸", "面部"},
	{"recommendation", "recommender", "推荐"},
	{"retrieval", "retrieve", "检索"},
	{"caption", "captioning", "字幕", "标注"},
	{"tracking", "跟踪"},

	// natural language of content
	{"chinese", "mandarin", "zh", "cn", "中文", "汉语", "华语"},
	{"english", "en", "英文", "英语"},
	{"japanese", "ja", "日语", "日文"},
	{"korean", "ko", "韩语", "韩文"},
	{"french", "fr", "法语", "法文"},
	{"german", "de", "德语", "德文"},
	{"spanish", "es", "西班牙语", "西语"},
	{"russian", "ru", "俄语", "俄文"},
	{"multilingual", "multi-lingual", "多语言", "多语种"},

	// domain
	{"medical", "health", "clinical", "医疗", "医学", "临床"},
	{"finance", "financial", "stock", "金融", "财经", "股票"},
	{"legal", "law", "法律", "法务"},
	{"code", "codes", "programming", "代码", "编程", "程序"},
	{"math", "mathematics", "数学"},
	{"science", "scientific", "科学"},
	{"news", "新闻"},
	{"movie", "movies", "film", "films", "电影", "影片", "影评"},
	{"music", "音乐"},
	{"book", "books", "书籍", "图书"},
	{"review", "reviews", "comment", "comments", "评论", "评价"},
	{"benchmark", "benchmarks", "evaluation", "基准", "评测", "评估"},
	{"paper", "papers", "academic", "论文", "学术"},
	{"security", "安全"},
	{"government", "政务", "政府"},

	// file format
	{"csv"}, {"json", "jsonl"}, {"parquet"}, {"excel", "xls", "xlsx", "表格"},
	{"xml"}, {"markdown", "md"},

	// misc
	{"train", "training", "训练"},
	{"test", "testing", "测试"},
	{"synthetic", "合成"},
	{"tiny", "small", "小型", "小样本"},
	{"large", "大规模", "大型"},
}

// synonymIndex maps every term to its full equivalence group.
var synonymIndex = func() map[string][]string {
	idx := make(map[string][]string)
	for _, group := range querySynonymGroups {
		for _, term := range group {
			idx[term] = group
		}
	}
	return idx
}()

// synonymKeysByLen holds all index keys sorted longest-first for greedy
// segmentation of unspaced Chinese queries like "图像分类".
var synonymKeysByLen = func() []string {
	keys := make([]string, 0, len(synonymIndex))
	for k := range synonymIndex {
		keys = append(keys, k)
	}
	// sort by rune length desc, then lexicographically for determinism
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			ri, rj := len([]rune(keys[i])), len([]rune(keys[j]))
			if rj > ri || (rj == ri && keys[j] < keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}()

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

// splitQueryTokens splits raw keywords on whitespace and punctuation.
func splitQueryTokens(keywords string) []string {
	return strings.FieldsFunc(keywords, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
}

// segmentCJK greedily splits an unspaced (mostly Chinese) token against the
// synonym keys, e.g. "图像分类" -> ["图像", "分类"]. Consecutive unmatched
// runes are merged into one raw term. Returns nil when no key matches,
// meaning the token should be used as-is.
func segmentCJK(token string) []string {
	var (
		parts   []string
		pending strings.Builder
		matched bool
	)
	flush := func() {
		if pending.Len() > 0 {
			parts = append(parts, pending.String())
			pending.Reset()
		}
	}
	rest := token
	for len(rest) > 0 {
		hit := ""
		for _, key := range segmentKeysByLen {
			if strings.HasPrefix(rest, key) {
				hit = key
				break
			}
		}
		if hit == "" {
			r, size := utf8.DecodeRuneInString(rest)
			pending.WriteRune(r)
			rest = rest[size:]
			continue
		}
		matched = true
		flush()
		parts = append(parts, hit)
		rest = rest[len(hit):]
	}
	flush()
	if !matched {
		return nil
	}
	return parts
}

// expandToken returns the synonym group for a token, or the token alone when
// no group contains it.
func expandToken(token string) []string {
	if group, ok := synonymIndex[token]; ok {
		return group
	}
	return []string{token}
}

// contentLangCode maps a language filter value onto the detected content
// language column (zh/en) by reusing the language synonym groups. Returns ""
// when the value has no structured equivalent (e.g. french) — full-text
// matching remains the fallback for those.
func contentLangCode(language string) string {
	group := expandToken(strings.ToLower(strings.TrimSpace(language)))
	switch {
	case slices.Contains(group, "chinese"):
		return "zh"
	case slices.Contains(group, "english"):
		return "en"
	}
	return ""
}

// stripContentLangTerms removes keyword tokens that merely name the language
// already enforced by the structured content_lang filter (e.g. "english" in
// "English text classification" when Language=english). LLM parsers often
// repeat the language inside keywords; keeping it as a full-text group would
// require documents to literally contain the word "english", which real
// English datasets never do. Tokens naming a *different* language are kept.
func stripContentLangTerms(keywords string, contentLang string) string {
	marker := map[string]string{"zh": "chinese", "en": "english"}[contentLang]
	if marker == "" {
		return keywords
	}
	var kept []string
	for _, token := range splitQueryTokens(strings.ToLower(keywords)) {
		if token == "" {
			continue
		}
		segments := segmentCJK(token)
		if segments == nil {
			segments = []string{token}
		}
		for _, seg := range segments {
			if slices.Contains(expandToken(seg), marker) {
				continue
			}
			kept = append(kept, seg)
		}
	}
	return strings.Join(kept, " ")
}

// HasSearchTerms reports whether the raw message contains at least one term
// from the synonym vocabulary (e.g. 中文, 视频, mit). Unknown raw words do
// not count — "现在有哪些数据集" must stay a browse query. Used to catch LLM
// misclassifications where a criteria-bearing message like "中文数据集有哪些"
// is labeled browse with empty filters.
func HasSearchTerms(message string) bool {
	for _, token := range splitQueryTokens(strings.ToLower(message)) {
		if token == "" || queryStopwords[token] {
			continue
		}
		if segments := segmentCJK(token); segments != nil {
			for _, seg := range segments {
				if _, ok := synonymIndex[seg]; ok {
					return true
				}
			}
			continue
		}
		if _, ok := synonymIndex[token]; ok {
			return true
		}
	}
	return false
}

// queryStopwords are tokens that carry no filtering meaning in a dataset
// search context (the user is already searching datasets). They are dropped
// so "语音数据集" behaves like "语音". Question phrasings are stopwords too:
// "中文数据集有哪些" must reduce to "中文".
var queryStopwords = map[string]bool{
	"数据集":      true,
	"数据":       true,
	"dataset":  true,
	"datasets": true,
	"data":     true,
	"有哪些":      true,
	"有没有":      true,
	"哪些":       true,
	"什么":       true,
	"呢":        true,
	"吗":        true,
	"一下":       true,
	"推荐":       true,
}

// segmentKeysByLen extends synonymKeysByLen with the stopwords so unspaced
// Chinese queries like "中文数据集有哪些" segment the stopwords out cleanly
// instead of merging them into an unmatchable raw term. Stopword segments
// are dropped later by buildKeywordGroups.
var segmentKeysByLen = func() []string {
	keys := make([]string, 0, len(synonymKeysByLen)+len(queryStopwords))
	keys = append(keys, synonymKeysByLen...)
	for k := range queryStopwords {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			ri, rj := len([]rune(keys[i])), len([]rune(keys[j]))
			if rj > ri || (rj == ri && keys[j] < keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}()

// KeywordExpansion carries equivalent expressions for one source keyword,
// produced by the query-parsing LLM (e.g. 多选题 -> multiple choice,
// multiple-choice). Equivalents are OR-combined with the source term inside
// its query group, so a Chinese query can hit English README/description
// content without any hardcoded cross-lingual word list. Groups stay
// AND-combined, so the strict keyword semantics are unchanged.
type KeywordExpansion struct {
	Term        string   `json:"term" jsonschema:"the source keyword, as it appears in keywords"`
	Equivalents []string `json:"equivalents" jsonschema:"1-8 equivalent or synonymous expressions; include the common english form when the source term is not english"`
}

// maxEquivalentsPerTerm caps LLM-provided expansions so one term cannot blow
// up the tsquery with dozens of OR branches.
const maxEquivalentsPerTerm = 8

// sanitizeKeywordExpansions normalizes LLM- or caller-provided expansions:
// lowercases and trims, drops empty terms, stopwords and duplicates, and caps
// the equivalents per term. Invalid entries are dropped silently — expansion
// is a best-effort recall aid and must never fail the search.
func sanitizeKeywordExpansions(expansions []KeywordExpansion) []KeywordExpansion {
	var out []KeywordExpansion
	for _, exp := range expansions {
		term := strings.ToLower(strings.TrimSpace(exp.Term))
		if term == "" || queryStopwords[term] {
			continue
		}
		seen := map[string]bool{term: true}
		var equivalents []string
		for _, eq := range exp.Equivalents {
			eq = strings.ToLower(strings.TrimSpace(eq))
			if eq == "" || queryStopwords[eq] || seen[eq] {
				continue
			}
			seen[eq] = true
			equivalents = append(equivalents, eq)
			if len(equivalents) >= maxEquivalentsPerTerm {
				break
			}
		}
		if len(equivalents) == 0 {
			continue
		}
		out = append(out, KeywordExpansion{Term: term, Equivalents: equivalents})
	}
	return out
}

// buildKeywordGroups converts raw keywords into query term groups: one group
// per semantic term, synonyms OR-combined inside the group. Unspaced Chinese
// tokens are first segmented against the synonym keys; stopwords are dropped.
// LLM-provided expansions are merged into the group of their source term.
func buildKeywordGroups(keywords string, expansions []KeywordExpansion) [][]string {
	equivByTerm := make(map[string][]string)
	for _, exp := range sanitizeKeywordExpansions(expansions) {
		equivByTerm[exp.Term] = exp.Equivalents
	}
	var groups [][]string
	appendTerm := func(term, token string) {
		if term == "" || queryStopwords[term] {
			return
		}
		// expandToken returns the shared synonym group slice; clone before
		// appending so LLM equivalents never mutate the global word list.
		group := slices.Clone(expandToken(term))
		equivs := equivByTerm[term]
		if len(equivs) == 0 && term != token {
			// the LLM may key an expansion by the whole raw token (e.g.
			// 多选题数据集) while segmentation split it; honor that too.
			equivs = equivByTerm[token]
		}
		for _, eq := range equivs {
			if !slices.Contains(group, eq) {
				group = append(group, eq)
			}
		}
		groups = append(groups, group)
	}
	for _, token := range splitQueryTokens(strings.ToLower(keywords)) {
		if token == "" || queryStopwords[token] {
			continue
		}
		if segments := segmentCJK(token); segments != nil {
			for _, seg := range segments {
				appendTerm(seg, token)
			}
			continue
		}
		appendTerm(token, token)
	}
	return groups
}
