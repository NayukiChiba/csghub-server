package mcpserver

import (
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
	{"video", "videos", "视频", "视频数据"},
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
		for _, key := range synonymKeysByLen {
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

// queryStopwords are tokens that carry no filtering meaning in a dataset
// search context (the user is already searching datasets). They are dropped
// so "语音数据集" behaves like "语音".
var queryStopwords = map[string]bool{
	"数据集":      true,
	"数据":       true,
	"dataset":  true,
	"datasets": true,
	"data":     true,
}

// buildKeywordGroups converts raw keywords into query term groups: one group
// per semantic term, synonyms OR-combined inside the group. Unspaced Chinese
// tokens are first segmented against the synonym keys; stopwords are dropped.
func buildKeywordGroups(keywords string) [][]string {
	var groups [][]string
	appendTerm := func(term string) {
		if term == "" || queryStopwords[term] {
			return
		}
		groups = append(groups, expandToken(term))
	}
	for _, token := range splitQueryTokens(strings.ToLower(keywords)) {
		if token == "" || queryStopwords[token] {
			continue
		}
		if segments := segmentCJK(token); segments != nil {
			for _, seg := range segments {
				appendTerm(seg)
			}
			continue
		}
		appendTerm(token)
	}
	return groups
}
