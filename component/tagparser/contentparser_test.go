package tagparser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const readme = `
---
task_categories:
- text-generation
language:
- zh
tags:
- text-generation
- 'llm '
- casual-lm
- language-modeling
pretty_name: SkyPile-150B
size_categories:
- 100B<n<1T
---
# SkyPile-150B

## Dataset Summary
SkyPile-150B is a comprehensive, large-scale Chinese dataset specifically designed for the pre-training of large language models. It is derived from a broad array of publicly accessible Chinese Internet web pages. Rigorous filtering, extensive deduplication, and thorough sensitive data filtering have been employed to ensure its quality. Furthermore, we have utilized advanced tools such as fastText and BERT to filter out low-quality data.

The publicly accessible portion of the SkyPile-150B dataset encompasses approximately 233 million unique web pages, each containing an average of over 1,000 Chinese characters. In total, the dataset includes approximately 150 billion tokens and 620 gigabytes of plain text data.


## Language
The SkyPile-150B dataset is exclusively composed of Chinese data.
`

const actualMeta = `
task_categories:
- text-generation
language:
- zh
tags:
- text-generation
- 'llm '
- casual-lm
- language-modeling
pretty_name: SkyPile-150B
size_categories:
- 100B<n<1T
`

func TestTagParser_MetaText(t *testing.T) {
	testMeta := metaText(readme)
	if testMeta != actualMeta {
		t.Errorf("expected %s, got %s", actualMeta, testMeta)
		t.Fail()
	}
}

func TestTagParser_ExtractDescription(t *testing.T) {
	cases := []struct {
		name   string
		readme string
		want   string
	}{
		{
			name:   "strips frontmatter and takes first prose paragraph",
			readme: readme,
			want:   "SkyPile-150B is a comprehensive, large-scale Chinese dataset specifically designed for the pre-training of large language models. It is derived from a broad array of publicly accessible Chinese Internet web pages. Rigorous filtering, extensive deduplication, and thorough sensitive data filtering have been employed to ensure its quality. Furthermore, we have utilized advanced tools such as fastText and BERT to filter out low-quality data.",
		},
		{
			name:   "skips headings images html and tables",
			readme: "# Title\n\n![badge](x.svg)\n<center>hi</center>\n| a | b |\n\nReal description here.\n\nSecond paragraph.",
			want:   "Real description here.",
		},
		{
			name:   "no frontmatter uses whole readme",
			readme: "# T\n\nplain body text",
			want:   "plain body text",
		},
		{
			name:   "empty readme",
			readme: "",
			want:   "",
		},
		{
			name:   "only headings",
			readme: "---\ntask_categories:\n- x\n---\n# Just A Title\n",
			want:   "",
		},
		{
			name:   "truncates to 500 runes",
			readme: strings.Repeat("字", 600),
			want:   strings.Repeat("字", 500),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, ExtractDescription(c.readme))
		})
	}
}

func TestTagParser_MetaTags_TaskIDsBecomeTaskTags(t *testing.T) {
	// HF cards carry fine-grained task_ids next to broad task_categories;
	// both are task tags (e.g. multiple-choice under question-answering)
	const card = `
---
task_categories:
- question-answering
task_ids:
- multiple-choice
language:
- en
---
# Demo
`
	metaTags, err := MetaTags(card)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"question-answering", "multiple-choice"}, metaTags["task"])
	require.ElementsMatch(t, []string{"en"}, metaTags["language"])
}

func TestTagParser_MetaTags(t *testing.T) {
	metaTags, err := MetaTags(readme)
	require.Nil(t, err)
	require.Equal(t, 4, len(metaTags["task"]))
	require.ElementsMatch(
		t, []string{"text-generation", "llm", "casual-lm", "language-modeling"}, metaTags["task"],
	)
	require.Equal(t, 1, len(metaTags["language"]))
	require.ElementsMatch(
		t, []string{"zh"}, metaTags["language"],
	)
	require.Equal(t, 0, len(metaTags["tags"]))
	require.Equal(t, 1, len(metaTags["pretty_name"]))
	require.ElementsMatch(
		t, []string{"SkyPile-150B"}, metaTags["pretty_name"],
	)
	require.Equal(t, 1, len(metaTags["size"]))
	require.ElementsMatch(
		t, []string{"100B<n<1T"}, metaTags["size"],
	)
}
