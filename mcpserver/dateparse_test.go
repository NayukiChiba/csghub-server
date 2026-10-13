package mcpserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixed "now" for deterministic date tests: 2026-10-13 (Tuesday) 15:04 local.
var dateTestNow = time.Date(2026, 10, 13, 15, 4, 5, 0, time.Local)

func requireDates(t *testing.T, c *dateConstraint, after, before string, updated bool) {
	t.Helper()
	require.NotNil(t, c)
	require.Equal(t, updated, c.updated)
	if after == "" {
		require.Nil(t, c.after)
	} else {
		require.NotNil(t, c.after)
		require.Equal(t, after, c.after.Format(time.DateOnly))
	}
	if before == "" {
		require.Nil(t, c.before)
	} else {
		require.NotNil(t, c.before)
		require.Equal(t, before, c.before.Format(time.DateOnly))
	}
}

func TestExtractDateConstraint_AbsoluteDates(t *testing.T) {
	// the motivating bug: "10.12" must deterministically be 2026-10-12
	requireDates(t, extractDateConstraint("10.12之后创建的数据集", dateTestNow), "2026-10-12", "", false)
	requireDates(t, extractDateConstraint("10.12 更新的数据集", dateTestNow), "2026-10-12", "2026-10-12", true)
	requireDates(t, extractDateConstraint("10月12日之后更新的数据集", dateTestNow), "2026-10-12", "", true)
	requireDates(t, extractDateConstraint("2026-10-12之后上传的", dateTestNow), "2026-10-12", "", false)
	requireDates(t, extractDateConstraint("2026年10月12号的数据集", dateTestNow), "2026-10-12", "2026-10-12", false)
	requireDates(t, extractDateConstraint("10/12以来的数据集", dateTestNow), "2026-10-12", "", false)
	requireDates(t, extractDateConstraint("10月12日之前发布的数据集", dateTestNow), "", "2026-10-12", false)
}

func TestExtractDateConstraint_YearRollover(t *testing.T) {
	// a month-day still in the future means last year ("12.30之后" in October)
	requireDates(t, extractDateConstraint("12.30之后的数据集", dateTestNow), "2025-12-30", "", false)
}

func TestExtractDateConstraint_Relative(t *testing.T) {
	requireDates(t, extractDateConstraint("今天更新的数据集", dateTestNow), "2026-10-13", "2026-10-13", true)
	requireDates(t, extractDateConstraint("昨天创建的数据集", dateTestNow), "2026-10-12", "2026-10-12", false)
	requireDates(t, extractDateConstraint("前天上传的", dateTestNow), "2026-10-11", "2026-10-11", false)
	requireDates(t, extractDateConstraint("3天前更新的", dateTestNow), "2026-10-10", "2026-10-10", true)
	requireDates(t, extractDateConstraint("三天前更新的", dateTestNow), "2026-10-10", "2026-10-10", true)
	requireDates(t, extractDateConstraint("最近3天的数据集", dateTestNow), "2026-10-10", "", false)
	requireDates(t, extractDateConstraint("近一周更新的", dateTestNow), "2026-10-06", "", true)
	requireDates(t, extractDateConstraint("最近2个月创建的数据集", dateTestNow), "2026-08-13", "", false)
	requireDates(t, extractDateConstraint("去年上传的数据集", dateTestNow), "2025-01-01", "2025-12-31", false)
	requireDates(t, extractDateConstraint("今年更新的", dateTestNow), "2026-01-01", "", true)
}

func TestExtractDateConstraint_Ranges(t *testing.T) {
	requireDates(t, extractDateConstraint("2026-10-01到2026-10-12之间更新的数据集", dateTestNow), "2026-10-01", "2026-10-12", true)
	// reversed endpoints are normalized
	requireDates(t, extractDateConstraint("2026-10-12 至 2026-10-01 创建的", dateTestNow), "2026-10-01", "2026-10-12", false)
}

func TestExtractDateConstraint_NoneForFuzzyOrMissing(t *testing.T) {
	require.Nil(t, extractDateConstraint("最近更新的数据集", dateTestNow))
	require.Nil(t, extractDateConstraint("有哪些数据集", dateTestNow))
	require.Nil(t, extractDateConstraint("多选题数据集", dateTestNow))
}

func TestExtractDateConstraint_UsesOnlyLatestQuestion(t *testing.T) {
	// date expressions in folded history must not leak into the current turn
	ctx := "对话上下文：\n用户：10.12之后创建的数据集\n助手：为您找到 3 个\n当前问题：有哪些语音数据集"
	require.Nil(t, extractDateConstraint(ctx, dateTestNow))
	ctx = "对话上下文：\n用户：现在有哪些数据集\n助手：平台目前共有 15 个数据集\n当前问题：10.12之后更新的呢"
	requireDates(t, extractDateConstraint(ctx, dateTestNow), "2026-10-12", "", true)
}

func TestParseCNNumber(t *testing.T) {
	for s, want := range map[string]int{"3": 3, "一": 1, "两": 2, "九": 9, "十": 10, "十二": 12, "二十": 20, "二十三": 23} {
		n, ok := parseCNNumber(s)
		require.True(t, ok, s)
		require.Equal(t, want, n, s)
	}
	_, ok := parseCNNumber("一百")
	require.False(t, ok)
}
