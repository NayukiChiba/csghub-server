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

func TestAllTagsHard(t *testing.T) {
	require.False(t, allTagsHard(nil))
	require.True(t, allTagsHard([]TagArg{{Hard: true}}))
	require.False(t, allTagsHard([]TagArg{{Hard: true}, {}}))
}
