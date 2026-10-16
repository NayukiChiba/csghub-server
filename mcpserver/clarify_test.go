package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClarifyScopeQuestion(t *testing.T) {
	history := []string{"我想要用于多模态的数据集", "好吧，有多选题数据吗？"}

	t.Run("scopeless zero-result follow-up triggers clarification", func(t *testing.T) {
		q := ClarifyScopeQuestion(history, "这里面哪个数据规模最大？", &ParseDatasetQueryResult{
			Sort: "largest",
			Tags: []TagArg{{Category: "task", Name: "multiple-choice"}}, // soft LLM tag
		})
		require.NotNil(t, q)
		require.Len(t, q.Options, 2)
		// the scoped option rephrases the question inside the last topic so
		// the next turn parses deterministically
		require.Contains(t, q.Options[0].Label, "多选题")
		require.Equal(t, "多选题数据集中，这里面哪个数据规模最大？", q.Options[0].Send)
		require.Equal(t, "所有数据集中，这里面哪个数据规模最大？", q.Options[1].Send)
	})

	t.Run("hard constraint stated verbatim stays an honest zero", func(t *testing.T) {
		// "它们里面有英文的吗" — the language is the user's own word
		q := ClarifyScopeQuestion(history, "它们里面有英文的吗", &ParseDatasetQueryResult{
			Language: "english", LanguageHard: true,
		})
		require.Nil(t, q)
	})

	t.Run("message with its own topic needs no clarification", func(t *testing.T) {
		q := ClarifyScopeQuestion(history, "那语音的呢", &ParseDatasetQueryResult{Modality: "audio", ModalityHard: true})
		require.Nil(t, q)
	})

	t.Run("date/size constraints make the zero trustworthy", func(t *testing.T) {
		q := ClarifyScopeQuestion(history, "10.12之后上传的有哪些", &ParseDatasetQueryResult{CreatedAfter: "2026-10-12"})
		require.Nil(t, q)
	})

	t.Run("no topical history means nothing to clarify against", func(t *testing.T) {
		q := ClarifyScopeQuestion([]string{"最大的是谁"}, "哪个最小", &ParseDatasetQueryResult{Sort: "smallest"})
		require.Nil(t, q)
		q = ClarifyScopeQuestion(nil, "哪个最小", &ParseDatasetQueryResult{Sort: "smallest"})
		require.Nil(t, q)
	})
}

func TestInheritHistoryConstraints_StopsAtTopicAnchor(t *testing.T) {
	// 多模态 belongs to the superseded turn-1 topic; the follow-up after the
	// turn-2 topic switch must not inherit it.
	p := &ParseDatasetQueryResult{Sort: "largest"}
	inheritHistoryConstraints(p, "哪个最大", []string{"我想要用于多模态的数据集", "有多选题数据吗"}, nil)
	require.Empty(t, p.Modality)

	// constraints from the current segment (the anchor message itself
	// included) still inherit; the scan stops after the anchor
	p = &ParseDatasetQueryResult{Sort: "largest"}
	inheritHistoryConstraints(p, "哪个最大", []string{"语音数据集有哪些", "小于10kb的呢"}, nil)
	require.NotNil(t, p.MaxSizeBytes)
	require.Equal(t, "audio", p.Modality)
}
