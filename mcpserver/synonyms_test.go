package mcpserver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildKeywordGroups_EnglishTokenExpands(t *testing.T) {
	groups := buildKeywordGroups("image", nil)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "image")
	require.Contains(t, groups[0], "图像")
}

func TestBuildKeywordGroups_UnspacedChineseSegments(t *testing.T) {
	groups := buildKeywordGroups("图像分类", nil)
	require.Len(t, groups, 2)
	require.Contains(t, groups[0], "image")
	require.Contains(t, groups[1], "classification")
}

func TestBuildKeywordGroups_MixedAndUnknown(t *testing.T) {
	groups := buildKeywordGroups("语音 mars rover", nil)
	require.Len(t, groups, 3)
	require.Contains(t, groups[0], "audio")
	require.Equal(t, []string{"mars"}, groups[1])
	require.Equal(t, []string{"rover"}, groups[2])
}

func TestBuildKeywordGroups_PartialChineseMatchKeepsRemainder(t *testing.T) {
	// "样本" is not in the synonym table; it should survive as a raw term
	groups := buildKeywordGroups("图像样本", nil)
	require.Len(t, groups, 2)
	require.Contains(t, groups[0], "image")
	require.Equal(t, []string{"样本"}, groups[1])
}

func TestBuildKeywordGroups_Empty(t *testing.T) {
	require.Empty(t, buildKeywordGroups("", nil))
	require.Empty(t, buildKeywordGroups("  , ", nil))
}

func TestBuildKeywordGroups_StopwordsDropped(t *testing.T) {
	// "数据集" carries no filtering meaning; "语音数据集" behaves like "语音"
	groups := buildKeywordGroups("语音数据集", nil)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "audio")
	require.Empty(t, buildKeywordGroups("数据集", nil))
	require.Empty(t, buildKeywordGroups("dataset data", nil))
}

func TestBuildKeywordGroups_VideoDatasetSegmentsToVideoOnly(t *testing.T) {
	// "视频数据集" must reduce to the video group alone; a "视频数据" synonym
	// key would segment to 视频+数据 and "数据" matches every Chinese
	// description, drowning the real video datasets.
	groups := buildKeywordGroups("视频数据集", nil)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "video")
	require.Contains(t, groups[0], "视频")
	require.NotContains(t, groups[0], "视频数据")
}

func TestExpandToken_UnknownKeepsToken(t *testing.T) {
	require.Equal(t, []string{"librispeech"}, expandToken("librispeech"))
}

func TestBuildKeywordGroups_QuestionPhrasingsDropped(t *testing.T) {
	// question phrasings are stopwords and segment out of unspaced Chinese
	groups := buildKeywordGroups("中文数据集有哪些", nil)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "chinese")
	require.Empty(t, buildKeywordGroups("有哪些数据集", nil))
	require.Empty(t, buildKeywordGroups("有没有什么数据集", nil))
}

func TestHasSearchTerms(t *testing.T) {
	// known domain terms count even when wrapped in question phrasing
	require.True(t, HasSearchTerms("中文数据集有哪些"))
	require.True(t, HasSearchTerms("有没有视频的数据集"))
	// pure browse phrasings do not — unknown words are not search terms
	require.False(t, HasSearchTerms("有哪些数据集"))
	require.False(t, HasSearchTerms("现在有哪些数据集"))
	require.False(t, HasSearchTerms("what datasets are available"))
}

func TestBuildKeywordGroups_LLMExpansionMergesIntoTermGroup(t *testing.T) {
	// whole-token hit: "多选题" is not in the synonym table; the LLM
	// expansion must widen its group with the english equivalents
	expansions := []KeywordExpansion{{Term: "多选题", Equivalents: []string{"multiple choice", "multiple-choice", "mcq"}}}
	groups := buildKeywordGroups("多选题", expansions)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "多选题")
	require.Contains(t, groups[0], "multiple choice")
	require.Contains(t, groups[0], "multiple-choice")

	// segmented hit: "多选题数据集" segments to 多选题 (stopword 数据集 dropped),
	// and an expansion keyed by the whole raw token is honored too
	groups = buildKeywordGroups("多选题数据集", expansions)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "multiple choice")
	groups = buildKeywordGroups("多选题数据集", []KeywordExpansion{{Term: "多选题数据集", Equivalents: []string{"multiple choice"}}})
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "multiple choice")
}

func TestBuildKeywordGroups_ExpansionOnSynonymTermKeepsGroup(t *testing.T) {
	// a term already in the synonym table keeps its group and gains the
	// equivalent as an extra OR branch
	expansions := []KeywordExpansion{{Term: "图像", Equivalents: []string{"picture"}}}
	groups := buildKeywordGroups("图像", expansions)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "image")
	require.Contains(t, groups[0], "picture")
	// duplicates already in the synonym group are not repeated
	groups = buildKeywordGroups("图像", []KeywordExpansion{{Term: "图像", Equivalents: []string{"image"}}})
	require.Len(t, groups, 1)
	require.Equal(t, 1, countOccurrences(groups[0], "image"))
}

func TestBuildKeywordGroups_ExpansionDoesNotMutateSynonymTable(t *testing.T) {
	expansions := []KeywordExpansion{{Term: "图像", Equivalents: []string{"llm-invented-word"}}}
	buildKeywordGroups("图像", expansions)
	// the global synonym group must be untouched by the LLM-injected term
	require.NotContains(t, expandToken("图像"), "llm-invented-word")
}

func TestSanitizeKeywordExpansions(t *testing.T) {
	out := sanitizeKeywordExpansions([]KeywordExpansion{
		{Term: " 多选题 ", Equivalents: []string{" Multiple Choice ", "multiple choice", "多选题", "", "数据集"}},
		{Term: "", Equivalents: []string{"x"}},
		{Term: "数据集", Equivalents: []string{"x"}},        // stopword term
		{Term: "火星", Equivalents: []string{"数据集", "  "}}, // no usable equivalents
	})
	require.Len(t, out, 1)
	require.Equal(t, "多选题", out[0].Term)
	require.Equal(t, []string{"multiple choice"}, out[0].Equivalents)
}

func TestSanitizeKeywordExpansions_CapsEquivalents(t *testing.T) {
	equivalents := make([]string, 0, maxEquivalentsPerTerm+3)
	for i := 0; i < maxEquivalentsPerTerm+3; i++ {
		equivalents = append(equivalents, fmt.Sprintf("eq%d", i))
	}
	out := sanitizeKeywordExpansions([]KeywordExpansion{{Term: "term", Equivalents: equivalents}})
	require.Len(t, out, 1)
	require.Len(t, out[0].Equivalents, maxEquivalentsPerTerm)
}

func countOccurrences(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}

func TestStripContentLangTerms(t *testing.T) {
	// the LLM often repeats the language inside keywords; the structured
	// content_lang filter already covers it
	require.Equal(t, "text classification", stripContentLangTerms("English text classification", "en"))
	require.Equal(t, "文本 分类", stripContentLangTerms("英文文本分类", "en"))
	require.Equal(t, "", stripContentLangTerms("english", "en"))
	// a different language mention is a real criterion and stays
	require.Equal(t, "english 语料", stripContentLangTerms("english 语料", "zh"))
	// unknown content lang: keywords untouched
	require.Equal(t, "french reviews", stripContentLangTerms("french reviews", ""))
}
