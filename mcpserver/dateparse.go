package mcpserver

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dateConstraint is a deterministic date filter extracted from the user
// query. Dates must not rely on LLM arithmetic — a compact form like "10.12"
// is occasionally misread by the model (e.g. as February 12), so explicit
// date expressions are parsed here and override the LLM's date fields.
type dateConstraint struct {
	after   *time.Time
	before  *time.Time
	updated bool // true targets updated_at, false targets created_at
}

var (
	// 2026-10-12 / 2026/10/12 / 2026.10.12 / 2026年10月12日(号)
	reFullDate = regexp.MustCompile(`(\d{4})\s*[-/.年]\s*(\d{1,2})\s*[-/.月]\s*(\d{1,2})\s*[日号]?`)
	// 10月12日(号) — must not be preceded by a year digit run
	reCnMonthDay = regexp.MustCompile(`(\d{1,2})\s*月\s*(\d{1,2})\s*[日号]`)
	// 10.12 / 10/12 / 10-12 — compact month-day, guarded against longer digit runs
	reMonthDay = regexp.MustCompile(`(?:^|[^\d.])(\d{1,2})\s*[-/.]\s*(\d{1,2})(?:[日号]|[^\d.]|$)`)
	// 3天前 / 三天前
	reDaysAgo = regexp.MustCompile(`(\d+|[一二两三四五六七八九十]{1,3})\s*天前`)
	// 最近3天 / 近三天
	reLastNDays   = regexp.MustCompile(`最?近\s*(\d+|[一二两三四五六七八九十]{1,3})\s*天`)
	reLastNWeeks  = regexp.MustCompile(`最?近\s*(\d+|[一二两三四五六七八九十]{1,3})\s*(?:个)?周`)
	reLastNMonths = regexp.MustCompile(`最?近\s*(\d+|[一二两三四五六七八九十]{1,3})\s*个?月`)
	reLastNYears  = regexp.MustCompile(`最?近\s*(\d+|[一二两三四五六七八九十]{1,3})\s*年`)
	reRangeSep    = regexp.MustCompile(`到|至|~|—|–|－|之间`)
)

// startOfDay returns t at local midnight.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// endOfDay returns t at 23:59:59.999... local time.
func endOfDay(t time.Time) time.Time {
	return startOfDay(t).Add(24*time.Hour - time.Nanosecond)
}

var cnDigits = map[rune]int{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// parseCNNumber parses an arabic or short Chinese numeral (一..九十九).
func parseCNNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	r := []rune(s)
	switch len(r) {
	case 1:
		if r[0] == '十' {
			return 10, true
		}
		if n, ok := cnDigits[r[0]]; ok {
			return n, true
		}
	case 2:
		if r[0] == '十' { // 十二
			if n, ok := cnDigits[r[1]]; ok {
				return 10 + n, true
			}
		}
		if r[1] == '十' { // 二十
			if n, ok := cnDigits[r[0]]; ok {
				return n * 10, true
			}
		}
	case 3:
		if r[1] == '十' { // 二十三
			tens, ok1 := cnDigits[r[0]]
			ones, ok2 := cnDigits[r[2]]
			if ok1 && ok2 {
				return tens*10 + ones, true
			}
		}
	}
	return 0, false
}

// latestQuestion strips folded conversation history from a contextualized
// parse query so date expressions in earlier turns do not leak into the
// current question's constraint.
func latestQuestion(query string) string {
	if idx := strings.LastIndex(query, "当前问题："); idx >= 0 {
		return query[idx+len("当前问题："):]
	}
	return query
}

// monthDayDate builds a date for a month-day without a year: current year,
// rolled back one year when that day is still in the future ("12.30之后"
// asked in October almost surely means last December).
func monthDayDate(now time.Time, month, day int) (time.Time, bool) {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	d := time.Date(now.Year(), time.Month(month), day, 0, 0, 0, 0, now.Location())
	if d.Day() != day { // e.g. Feb 31 rolled into March
		return time.Time{}, false
	}
	if d.After(now) {
		d = d.AddDate(-1, 0, 0)
	}
	return d, true
}

// dayWindow returns [startOfDay, endOfDay] for a single day.
func dayWindow(day time.Time) (*time.Time, *time.Time) {
	start := startOfDay(day)
	end := endOfDay(day)
	return &start, &end
}

// extractDateConstraint deterministically parses date expressions out of the
// (possibly history-folded) query. Returns nil when nothing is recognized,
// leaving the LLM's date fields in place for fuzzy phrasings.
func extractDateConstraint(query string, now time.Time) *dateConstraint {
	q := latestQuestion(query)
	c := &dateConstraint{}
	if strings.Contains(q, "更新") {
		c.updated = true
	}

	// relative expressions first: they are unambiguous and win over any
	// month-day digits that happen to appear nearby
	if strings.Contains(q, "前天") {
		c.after, c.before = dayWindow(now.AddDate(0, 0, -2))
		return c
	}
	if strings.Contains(q, "昨天") || strings.Contains(q, "昨日") {
		c.after, c.before = dayWindow(now.AddDate(0, 0, -1))
		return c
	}
	if strings.Contains(q, "今天") || strings.Contains(q, "今日") {
		c.after, c.before = dayWindow(now)
		return c
	}
	if m := reDaysAgo.FindStringSubmatch(q); m != nil {
		if n, ok := parseCNNumber(m[1]); ok && n > 0 {
			c.after, c.before = dayWindow(now.AddDate(0, 0, -n))
			return c
		}
	}
	if m := reLastNDays.FindStringSubmatch(q); m != nil {
		if n, ok := parseCNNumber(m[1]); ok && n > 0 {
			after := startOfDay(now.AddDate(0, 0, -n))
			c.after = &after
			return c
		}
	}
	if m := reLastNWeeks.FindStringSubmatch(q); m != nil {
		if n, ok := parseCNNumber(m[1]); ok && n > 0 {
			after := startOfDay(now.AddDate(0, 0, -7*n))
			c.after = &after
			return c
		}
	}
	if m := reLastNMonths.FindStringSubmatch(q); m != nil {
		if n, ok := parseCNNumber(m[1]); ok && n > 0 {
			after := startOfDay(now.AddDate(0, -n, 0))
			c.after = &after
			return c
		}
	}
	if m := reLastNYears.FindStringSubmatch(q); m != nil {
		if n, ok := parseCNNumber(m[1]); ok && n > 0 {
			after := startOfDay(now.AddDate(-n, 0, 0))
			c.after = &after
			return c
		}
	}
	if strings.Contains(q, "上周") || strings.Contains(q, "上星期") {
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		thisMonday := startOfDay(now.AddDate(0, 0, -(weekday - 1)))
		after := thisMonday.AddDate(0, 0, -7)
		before := endOfDay(thisMonday.AddDate(0, 0, -1))
		c.after, c.before = &after, &before
		return c
	}
	if strings.Contains(q, "本周") || strings.Contains(q, "这周") || strings.Contains(q, "本星期") {
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		after := startOfDay(now.AddDate(0, 0, -(weekday - 1)))
		c.after = &after
		return c
	}
	if strings.Contains(q, "上个月") || strings.Contains(q, "上月") {
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		after := first.AddDate(0, -1, 0)
		before := endOfDay(first.Add(-time.Nanosecond))
		c.after, c.before = &after, &before
		return c
	}
	if strings.Contains(q, "本月") || strings.Contains(q, "这个月") {
		after := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		c.after = &after
		return c
	}
	if strings.Contains(q, "去年") {
		after := time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, now.Location())
		before := endOfDay(time.Date(now.Year()-1, 12, 31, 0, 0, 0, 0, now.Location()))
		c.after, c.before = &after, &before
		return c
	}
	if strings.Contains(q, "今年") {
		after := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
		c.after = &after
		return c
	}

	// absolute dates: collect up to two; two dates with a range separator
	// between them form an interval
	var days []time.Time
	if locs := reFullDate.FindAllStringSubmatchIndex(q, 2); locs != nil {
		for _, loc := range locs {
			y, _ := strconv.Atoi(q[loc[2]:loc[3]])
			mo, _ := strconv.Atoi(q[loc[4]:loc[5]])
			dy, _ := strconv.Atoi(q[loc[6]:loc[7]])
			d := time.Date(y, time.Month(mo), dy, 0, 0, 0, 0, now.Location())
			if mo < 1 || mo > 12 || dy < 1 || dy > 31 || d.Month() != time.Month(mo) || d.Day() != dy {
				return nil
			}
			days = append(days, d)
		}
	} else {
		if loc := reCnMonthDay.FindStringSubmatchIndex(q); loc != nil {
			mo, _ := strconv.Atoi(q[loc[2]:loc[3]])
			dy, _ := strconv.Atoi(q[loc[4]:loc[5]])
			if d, ok := monthDayDate(now, mo, dy); ok {
				days = append(days, d)
			} else {
				return nil
			}
		}
		if loc := reMonthDay.FindStringSubmatchIndex(q); len(days) < 2 && loc != nil {
			mo, _ := strconv.Atoi(q[loc[2]:loc[3]])
			dy, _ := strconv.Atoi(q[loc[4]:loc[5]])
			if d, ok := monthDayDate(now, mo, dy); ok {
				days = append(days, d)
			}
		}
	}
	if len(days) == 0 {
		return nil
	}
	if len(days) == 2 && reRangeSep.MatchString(q) {
		after, before := days[0], days[1]
		if before.Before(after) {
			after, before = before, after
		}
		start := startOfDay(after)
		end := endOfDay(before)
		c.after, c.before = &start, &end
		return c
	}
	day := days[0]
	switch {
	case strings.Contains(q, "之前") || strings.Contains(q, "以前"):
		end := endOfDay(day)
		c.before = &end
	case strings.Contains(q, "之后") || strings.Contains(q, "以来") || strings.Contains(q, "起"):
		start := startOfDay(day)
		c.after = &start
	default:
		c.after, c.before = dayWindow(day)
	}
	return c
}
