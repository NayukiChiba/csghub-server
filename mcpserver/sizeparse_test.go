package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractSizeConstraint(t *testing.T) {
	cases := []struct {
		query   string
		wantMin int64
		wantMax int64
	}{
		{"小于10kb的数据集有哪些", 0, 10240},
		{"10kb以内的数据集", 0, 10240},
		{"不超过2MB的数据", 0, 2 * 1024 * 1024},
		{"1.5GB以下", 0, 1610612736},
		{"大于2MB的数据集", 2 * 1024 * 1024, 0},
		{"2mb以上的", 2 * 1024 * 1024, 0},
		{"至少500kb", 500 * 1024, 0},
		// 不少于/不低于/不超过 must not be read as their bare inner word
		{"不少于100kb", 100 * 1024, 0},
		{"不低于1MB的数据", 1024 * 1024, 0},
		{"less than 10kb", 0, 10240},
		{"datasets under 100MB", 0, 100 * 1024 * 1024},
		{"at least 500kb of text", 500 * 1024, 0},
		{"大于1kb 小于10kb", 1024, 10240},
		// the most restrictive bound wins when several appear
		{"小于10kb 且小于20kb", 0, 10240},
	}
	for _, c := range cases {
		sc := extractSizeConstraint(c.query)
		require.NotNil(t, sc, "query %q", c.query)
		if c.wantMin > 0 {
			require.NotNil(t, sc.min, "query %q min", c.query)
			require.Equal(t, c.wantMin, *sc.min, "query %q min", c.query)
		} else {
			require.Nil(t, sc.min, "query %q min", c.query)
		}
		if c.wantMax > 0 {
			require.NotNil(t, sc.max, "query %q max", c.query)
			require.Equal(t, c.wantMax, *sc.max, "query %q max", c.query)
		} else {
			require.Nil(t, sc.max, "query %q max", c.query)
		}
	}
}

func TestExtractSizeConstraint_NoMatch(t *testing.T) {
	for _, q := range []string{
		"数据集有哪些",
		"小数据集",       // fuzzy phrasing without a number stays with the LLM
		"最大的数据集是谁",   // ordering word, not a bound
		"10.12之后上传的", // a date, not a size
		// folded history must not leak into the current question
		"对话上下文：\n用户：小于10kb的数据集\n当前问题：最大的是谁",
	} {
		require.Nil(t, extractSizeConstraint(q), "query %q", q)
	}
}

func TestApplySizeConstraint(t *testing.T) {
	t.Run("overrides the llm's size fields", func(t *testing.T) {
		wrong := int64(10000) // LLM used 1000-based units
		p := &ParseDatasetQueryResult{MaxSizeBytes: &wrong}
		applySizeConstraint(p, "小于10kb的数据集")
		require.Equal(t, int64(10240), *p.MaxSizeBytes)
		require.Nil(t, p.MinSizeBytes)
	})

	t.Run("keeps the llm's fields when nothing is recognized", func(t *testing.T) {
		llm := int64(4096)
		p := &ParseDatasetQueryResult{MaxSizeBytes: &llm}
		applySizeConstraint(p, "小数据集")
		require.Equal(t, int64(4096), *p.MaxSizeBytes)
	})
}
