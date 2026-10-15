package mcpserver

import (
	"regexp"
	"strconv"
	"strings"
)

// sizeConstraint is a deterministic size filter extracted from the user
// query. Sizes must not rely on LLM arithmetic — unit conversion ("10kb" is
// 10240 bytes, not 10000) is occasionally fumbled by the model, so explicit
// size expressions are parsed here and override the LLM's size fields.
type sizeConstraint struct {
	min *int64
	max *int64
}

var (
	// comparator before the number: 小于10kb / 不超过2MB / less than 10kb.
	// 少于/低于/超过 must not match inside 不少于/不低于/不超过 — RE2 has no
	// lookbehind, so a leading rune class does the exclusion.
	reSizeMaxBefore = regexp.MustCompile(`(?i)(?:小于|(?:^|[^不])少于|(?:^|[^不])低于|不大于|不超过|不到|至多|最多|less than|no more than|at most|smaller than|under|below|within)\s*(\d+(?:\.\d+)?)\s*(kb|mb|gb|tb|b)\b`)
	reSizeMinBefore = regexp.MustCompile(`(?i)(?:大于|多于|高于|(?:^|[^不])超过|不低于|不少于|至少|最少|more than|at least|bigger than|larger than|over|exceeds?)\s*(\d+(?:\.\d+)?)\s*(kb|mb|gb|tb|b)\b`)
	// comparator after the number: 10kb以内 / 2MB以上 / 10kb or less
	reSizeMaxAfter = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(kb|mb|gb|tb|b)\s*(?:以内|以下|之内|及以下|or less|or smaller)`)
	reSizeMinAfter = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(kb|mb|gb|tb|b)\s*(?:以上|及以上|or more|or bigger|or larger)`)
)

// sizeUnitBytes maps a unit suffix onto its 1024-based byte multiplier.
func sizeUnitBytes(unit string) int64 {
	switch strings.ToLower(unit) {
	case "kb":
		return 1024
	case "mb":
		return 1024 * 1024
	case "gb":
		return 1024 * 1024 * 1024
	case "tb":
		return 1024 * 1024 * 1024 * 1024
	default: // "b"
		return 1
	}
}

// sizeMatchBytes converts one regex match (number, unit) to bytes.
func sizeMatchBytes(m []string, numIdx, unitIdx int) (int64, bool) {
	v, err := strconv.ParseFloat(m[numIdx], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return int64(v * float64(sizeUnitBytes(m[unitIdx]))), true
}

// extractSizeConstraint deterministically parses size expressions out of the
// (possibly history-folded) query. Returns nil when nothing is recognized,
// leaving the LLM's size fields in place for fuzzy phrasings ("小数据集"
// without a number stays soft).
func extractSizeConstraint(query string) *sizeConstraint {
	q := latestQuestion(query)
	c := &sizeConstraint{}
	applyMax := func(m []string, numIdx, unitIdx int) {
		if v, ok := sizeMatchBytes(m, numIdx, unitIdx); ok && v > 0 && (c.max == nil || v < *c.max) {
			c.max = &v
		}
	}
	applyMin := func(m []string, numIdx, unitIdx int) {
		if v, ok := sizeMatchBytes(m, numIdx, unitIdx); ok && v > 0 && (c.min == nil || v > *c.min) {
			c.min = &v
		}
	}
	// group layouts differ between the before/after forms
	for _, m := range reSizeMaxBefore.FindAllStringSubmatch(q, -1) {
		applyMax(m, 1, 2)
	}
	for _, m := range reSizeMinBefore.FindAllStringSubmatch(q, -1) {
		applyMin(m, 1, 2)
	}
	for _, m := range reSizeMaxAfter.FindAllStringSubmatch(q, -1) {
		applyMax(m, 1, 2)
	}
	for _, m := range reSizeMinAfter.FindAllStringSubmatch(q, -1) {
		applyMin(m, 1, 2)
	}
	if c.min == nil && c.max == nil {
		return nil
	}
	return c
}

// applySizeConstraint overrides the result's size fields with the
// deterministically parsed size expression, if the query contains one.
// Unrecognized fuzzy phrasings keep the LLM's interpretation.
func applySizeConstraint(parsed *ParseDatasetQueryResult, query string) {
	sc := extractSizeConstraint(query)
	if sc == nil {
		return
	}
	parsed.MinSizeBytes, parsed.MaxSizeBytes = sc.min, sc.max
}
