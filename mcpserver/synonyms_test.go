package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildKeywordGroups_EnglishTokenExpands(t *testing.T) {
	groups := buildKeywordGroups("image")
	require.Len(t, groups, 1)
	require.Contains(t, groups[0], "image")
	require.Contains(t, groups[0], "图像")
}

func TestBuildKeywordGroups_UnspacedChineseSegments(t *testing.T) {
	groups := buildKeywordGroups("图像分类")
	require.Len(t, groups, 2)
	require.Contains(t, groups[0], "image")
	require.Contains(t, groups[1], "classification")
}

func TestBuildKeywordGroups_MixedAndUnknown(t *testing.T) {
	groups := buildKeywordGroups("语音 mars rover")
	require.Len(t, groups, 3)
	require.Contains(t, groups[0], "audio")
	require.Equal(t, []string{"mars"}, groups[1])
	require.Equal(t, []string{"rover"}, groups[2])
}

func TestBuildKeywordGroups_PartialChineseMatchKeepsRemainder(t *testing.T) {
	// "样本" is not in the synonym table; it should survive as a raw term
	groups := buildKeywordGroups("图像样本")
	require.Len(t, groups, 2)
	require.Contains(t, groups[0], "image")
	require.Equal(t, []string{"样本"}, groups[1])
}

func TestBuildKeywordGroups_Empty(t *testing.T) {
	require.Empty(t, buildKeywordGroups(""))
	require.Empty(t, buildKeywordGroups("  , "))
}

func TestExpandToken_UnknownKeepsToken(t *testing.T) {
	require.Equal(t, []string{"librispeech"}, expandToken("librispeech"))
}
