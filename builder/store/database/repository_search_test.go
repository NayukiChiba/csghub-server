package database

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSerializeSearchGroups(t *testing.T) {
	got := serializeSearchGroups([][]string{
		{"image", "图像", "图片"},
		{"classification", "分类"},
	})
	require.Equal(t, "image|图像|图片\nclassification|分类", got)
}

func TestSerializeSearchGroups_StripsSeparatorsAndEmpties(t *testing.T) {
	got := serializeSearchGroups([][]string{
		{"a|b", "c\nd", "  "},
		{},
		{"x"},
	})
	require.Equal(t, "a b|c d\nx", got)
}
