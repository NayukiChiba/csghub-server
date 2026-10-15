package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/builder/store/database"
)

func TestExplicitModality(t *testing.T) {
	cases := map[string]string{
		"图像分类数据集":       "image",
		"那视频的呢？":        "video",
		"多模态评估数据集":      "multimodal",
		"有没有语音数据集":      "audio",
		"文本分类":          "text",
		"image dataset": "image",
		// no modality word present
		"今天上传的数据集":    "",
		"火星土壤样本":      "",
		"mit 许可证的数据集": "",
	}
	for msg, want := range cases {
		require.Equal(t, want, explicitModality(msg), "message %q", msg)
	}
}

func TestExplicitLanguage(t *testing.T) {
	cases := map[string]string{
		"中文数据集有哪些":       "chinese",
		"英文的文本分类":        "english",
		"english corpus": "english",
		"图像分类":           "",
		"10.12 上传的":      "",
	}
	for msg, want := range cases {
		require.Equal(t, want, explicitLanguage(msg), "message %q", msg)
	}
}

func TestMatchExplicitTags(t *testing.T) {
	vocab := []*database.Tag{
		{Category: "task", Name: "image-classification"},
		{Category: "task", Name: "text-classification"},
		{Category: "task", Name: "token-classification"},
		{Category: "task", Name: "multiple-choice"},
		{Category: "task", Name: "question-answering"},
		{Category: "license", Name: "mit"},
		// language tags are handled by the structured filter, never matched here
		{Category: "language", Name: "en"},
		// noisy categories are out of scope
		{Category: "evaluation", Name: "examination"},
	}
	cases := []struct {
		message string
		want    []TagArg
	}{
		{"图像分类数据集", []TagArg{{Category: "task", Name: "image-classification", Hard: true}}},
		{"适合文本分类的", []TagArg{{Category: "task", Name: "text-classification", Hard: true}}},
		// "choice"/"multiple" are covered through the multiple-choice synonym group
		{"有没有多选题数据集", []TagArg{{Category: "task", Name: "multiple-choice", Hard: true}}},
		{"mit 许可证的数据集", []TagArg{{Category: "license", Name: "mit", Hard: true}}},
		// 分类 alone covers only half of every *-classification tag
		{"分类", nil},
		{"语音数据集", nil},
		// 问答 covers question-answering via the qa synonym group
		{"问答数据集", []TagArg{{Category: "task", Name: "question-answering", Hard: true}}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, matchExplicitTags(c.message, vocab), "message %q", c.message)
	}
}

func TestApplyExplicitConstraints(t *testing.T) {
	vocab := []*database.Tag{{Category: "task", Name: "image-classification"}}

	t.Run("fills missing modality and marks it hard", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Keywords: "image classification"}
		applyExplicitConstraints(p, "图像分类数据集", vocab)
		require.Equal(t, "image", p.Modality)
		require.True(t, p.ModalityHard)
	})

	t.Run("user's word overrides the LLM's contradicting guess", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Modality: "text"}
		applyExplicitConstraints(p, "那视频的呢", nil)
		require.Equal(t, "video", p.Modality)
		require.True(t, p.ModalityHard)
	})

	t.Run("llm-guessed tag the user also said becomes hard, not duplicated", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Tags: []TagArg{{Category: "task", Name: "image-classification"}}}
		applyExplicitConstraints(p, "图像分类", vocab)
		require.Len(t, p.Tags, 1)
		require.True(t, p.Tags[0].Hard)
	})

	t.Run("soft llm tags stay soft", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Tags: []TagArg{{Category: "task", Name: "question-answering"}}}
		applyExplicitConstraints(p, "图像分类", vocab)
		require.Len(t, p.Tags, 2)
		require.False(t, allTagsHard(p.Tags))
	})

	t.Run("user-stated language is marked hard", func(t *testing.T) {
		p := &ParseDatasetQueryResult{}
		applyExplicitConstraints(p, "英文的文本分类", nil)
		require.Equal(t, "english", p.Language)
		require.True(t, p.LanguageHard)
	})

	t.Run("llm-guessed language stays soft", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Language: "chinese"}
		applyExplicitConstraints(p, "语音数据集", nil)
		require.Equal(t, "chinese", p.Language)
		require.False(t, p.LanguageHard)
	})
}

func TestInheritHistoryConstraints(t *testing.T) {
	vocab := []*database.Tag{{Category: "task", Name: "multiple-choice"}}

	t.Run("follow-up inherits the size bound from history", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Sort: "largest"}
		inheritHistoryConstraints(p, "最大的是谁", []string{"小于10kb的数据集有哪些"}, nil)
		require.NotNil(t, p.MaxSizeBytes)
		require.Equal(t, int64(10240), *p.MaxSizeBytes)
	})

	t.Run("current message's own bound wins", func(t *testing.T) {
		own := int64(20480)
		p := &ParseDatasetQueryResult{MaxSizeBytes: &own}
		inheritHistoryConstraints(p, "最大的是谁", []string{"小于10kb的数据集"}, nil)
		require.Equal(t, int64(20480), *p.MaxSizeBytes)
	})

	t.Run("modality is inherited as hard; the current one wins", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Sort: "largest"}
		inheritHistoryConstraints(p, "最大的是谁", []string{"语音数据集有哪些"}, nil)
		require.Equal(t, "audio", p.Modality)
		require.True(t, p.ModalityHard)

		p = &ParseDatasetQueryResult{Modality: "image", ModalityHard: true}
		inheritHistoryConstraints(p, "那视频的呢", []string{"语音数据集有哪些"}, nil)
		require.Equal(t, "image", p.Modality)
	})

	t.Run("language, tags and dates are inherited", func(t *testing.T) {
		p := &ParseDatasetQueryResult{}
		inheritHistoryConstraints(p, "最大的是谁", []string{"10.12之后上传的多选题数据集"}, vocab)
		require.Equal(t, []TagArg{{Category: "task", Name: "multiple-choice", Hard: true}}, p.Tags)
		require.Equal(t, "2026-10-12", p.CreatedAfter)

		p = &ParseDatasetQueryResult{}
		inheritHistoryConstraints(p, "最大的是谁", []string{"英文的文本分类"}, nil)
		require.Equal(t, "english", p.Language)
		require.True(t, p.LanguageHard)
	})

	t.Run("recognized topic terms are inherited as keywords", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Sort: "most_download"}
		inheritHistoryConstraints(p, "下载量最高的是谁", []string{"文本分类的簌俱寂有哪些"}, nil)
		require.Equal(t, "文本 分类", p.Keywords)
	})

	t.Run("keywords stay empty when the follow-up has its own terms or llm keywords", func(t *testing.T) {
		// 那视频的呢 switches topic: the inherited 图像/分类 terms must not
		// poison the video query
		p := &ParseDatasetQueryResult{Modality: "video", ModalityHard: true}
		inheritHistoryConstraints(p, "那视频的呢", []string{"图像分类数据集"}, nil)
		require.Empty(t, p.Keywords)

		p = &ParseDatasetQueryResult{Keywords: "llm supplied"}
		inheritHistoryConstraints(p, "最大的是谁", []string{"语音数据集"}, nil)
		require.Equal(t, "llm supplied", p.Keywords)
	})

	t.Run("unrecognized history text yields no keywords", func(t *testing.T) {
		p := &ParseDatasetQueryResult{}
		inheritHistoryConstraints(p, "最大的是谁", []string{"火星土壤样本数据集"}, nil)
		require.Empty(t, p.Keywords)
	})

	t.Run("llm-repeated history tag is upgraded to hard", func(t *testing.T) {
		p := &ParseDatasetQueryResult{Tags: []TagArg{{Category: "task", Name: "multiple-choice"}}}
		inheritHistoryConstraints(p, "下载量最高的呢", []string{"多选题数据集"}, vocab)
		require.Len(t, p.Tags, 1)
		require.True(t, p.Tags[0].Hard)
	})

	t.Run("topic switch drops stale llm-carried tags", func(t *testing.T) {
		// "那语音的呢" after an image search: the LLM copied the image tag
		// into the follow-up; the new topic must start clean instead of
		// pinning the audio query back to image datasets
		imageVocab := []*database.Tag{{Category: "task", Name: "image-classification"}}
		p := &ParseDatasetQueryResult{Tags: []TagArg{{Category: "task", Name: "image-classification"}}}
		inheritHistoryConstraints(p, "那语音的呢", []string{"图像分类数据集"}, imageVocab)
		require.Empty(t, p.Tags)

		// a tag the current message states explicitly survives the reset
		mcVocab := append([]*database.Tag{{Category: "task", Name: "multiple-choice"}}, imageVocab...)
		p = &ParseDatasetQueryResult{Tags: []TagArg{
			{Category: "task", Name: "image-classification"},
			{Category: "task", Name: "multiple-choice"},
		}}
		inheritHistoryConstraints(p, "那语音的多选题呢", []string{"图像分类数据集"}, mcVocab)
		require.Equal(t, []TagArg{{Category: "task", Name: "multiple-choice", Hard: true}}, p.Tags)
	})

	t.Run("language-only follow-up is not a topic switch", func(t *testing.T) {
		// "它们里面有英文的吗" refines the current results: history tags stay
		p := &ParseDatasetQueryResult{Tags: []TagArg{{Category: "task", Name: "multiple-choice"}}}
		inheritHistoryConstraints(p, "它们里面有英文的吗", []string{"多选题数据集"}, vocab)
		require.Len(t, p.Tags, 1)
		require.True(t, p.Tags[0].Hard)
	})

	t.Run("fallback keeps its raw keywords but still inherits filters", func(t *testing.T) {
		// the LLM is down for a follow-up: the raw message stays the keywords
		// while the deterministic history constraints keep the scope
		p := &ParseDatasetQueryResult{Fallback: true, Keywords: "最大的呢"}
		inheritHistoryConstraints(p, "最大的呢", []string{"小于10kb的数据集"}, nil)
		require.Equal(t, "最大的呢", p.Keywords)
		require.NotNil(t, p.MaxSizeBytes)
		require.Equal(t, int64(10240), *p.MaxSizeBytes)

		p = &ParseDatasetQueryResult{}
		inheritHistoryConstraints(p, "最大的是谁", nil, nil)
		require.Nil(t, p.MaxSizeBytes)
	})
}

func TestRecognizedTopicTerms(t *testing.T) {
	require.Equal(t, "文本 分类", recognizedTopicTerms("文本分类的簌俱寂有哪些"))
	require.Equal(t, "语音", recognizedTopicTerms("语音数据集"))
	require.Equal(t, "", recognizedTopicTerms("火星土壤样本数据集"))
	require.Equal(t, "", recognizedTopicTerms("小于10kb的数据集有哪些"))
	require.Equal(t, "", recognizedTopicTerms(""))
}

func TestHasNonLanguageTopicTerms(t *testing.T) {
	require.True(t, hasNonLanguageTopicTerms("那语音的呢"))
	require.True(t, hasNonLanguageTopicTerms("那视频的呢"))
	require.True(t, hasNonLanguageTopicTerms("英文的语音数据集"))
	// language-only phrasings refine the current scope, they do not switch
	require.False(t, hasNonLanguageTopicTerms("它们里面有英文的吗"))
	require.False(t, hasNonLanguageTopicTerms("有日语的吗"))
	require.False(t, hasNonLanguageTopicTerms("最大的是谁"))
	require.False(t, hasNonLanguageTopicTerms("火星土壤样本"))
}

func TestLanguageTagName(t *testing.T) {
	vocab := []*database.Tag{
		{Category: "task", Name: "text-classification"},
		{Category: "language", Name: "en"},
		{Category: "language", Name: "ja"},
	}
	require.Equal(t, "ja", languageTagName(vocab, "japanese"))
	require.Equal(t, "ja", languageTagName(vocab, "日语"))
	require.Equal(t, "en", languageTagName(vocab, "英文"))
	require.Equal(t, "", languageTagName(vocab, "法语"))
	require.Equal(t, "", languageTagName(vocab, "klingon"))
}

func TestStripLanguageTerms(t *testing.T) {
	require.Equal(t, "", stripLanguageTerms("日语", "japanese"))
	require.Equal(t, "分类", stripLanguageTerms("日文 分类", "日文"))
	// a different language is kept
	require.Equal(t, "中文", stripLanguageTerms("中文", "english"))
	require.Equal(t, "corpus", stripLanguageTerms("corpus", "french"))
}

func TestAllTagsHard(t *testing.T) {
	require.False(t, allTagsHard(nil))
	require.True(t, allTagsHard([]TagArg{{Hard: true}}))
	require.False(t, allTagsHard([]TagArg{{Hard: true}, {}}))
}
