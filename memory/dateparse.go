package memory

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 日期换算交给代码，不交给模型：0.8b 会把「下周三」算成明天、会把示例里的日期照抄过来。
// 能确定性算出来的就自己算；算不出来时由宿主回问用户，绝不猜一个日期出来。
//
// 覆盖的表述（按优先级）：
//   YYYY-MM-DD ／ 今天 明天 后天 大后天 ／ N 天后 ／ N 周后 ／ N 个月后
//   下下周X ／ 下周X ／ 这周X 周X 星期X 礼拜X ／ 下个月 N 号 ／ 月底 ／ 明年 M 月 D 日 ／ M 月 D 日

const numPattern = `(\d{1,3}|[一二三四五六七八九十两]+)`

var (
	isoDateRe     = regexp.MustCompile(`(\d{4})\s*-\s*(\d{1,2})\s*-\s*(\d{1,2})`)
	cnDateRe      = regexp.MustCompile(`(\d{1,2})\s*月\s*(\d{1,2})\s*[日号]`)
	nextMonthRe   = regexp.MustCompile(`下\s*个?\s*月\s*(\d{1,2})\s*[日号]`)
	daysLaterRe   = regexp.MustCompile(numPattern + `\s*(?:天|日)\s*(?:之?后|以后)`)
	weeksLaterRe  = regexp.MustCompile(numPattern + `\s*(?:周|星期|礼拜)\s*(?:之?后|以后)`)
	monthsLaterRe = regexp.MustCompile(numPattern + `\s*个\s*月\s*(?:之?后|以后)`)
	weekRe        = regexp.MustCompile(`(下下|下|这|本)?\s*(?:周|星期|礼拜)\s*([一二三四五六日天])`)
	endOfMonth    = regexp.MustCompile(`(?:月底|月末)`)
	clockRe       = regexp.MustCompile(`(\d{1,2})\s*[:：]\s*(\d{2})`)
	hourRe        = regexp.MustCompile(`(凌晨|早上|上午|中午|下午|傍晚|晚上|夜里|晚间)?\s*(\d{1,2}|[一二三四五六七八九十两]+)\s*点\s*(半|(\d{1,2}|[一二三四五六七八九十]+)\s*分?)?`)
)

var cnDigits = map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// numOf 解析 1-99 的数字：阿拉伯数字直接用，中文按「十」「二十」「三十五」这种常见写法。
func numOf(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	var total, cur int
	for _, r := range s {
		if r == '十' {
			if cur == 0 {
				cur = 1
			}
			total += cur * 10
			cur = 0
			continue
		}
		cur = cnDigits[r]
	}
	return total + cur
}

// weekdayOf 周一=1 … 周日=7。
func weekdayOf(r string) int {
	switch r {
	case "一":
		return 1
	case "二":
		return 2
	case "三":
		return 3
	case "四":
		return 4
	case "五":
		return 5
	case "六":
		return 6
	default: // 日 / 天
		return 7
	}
}

const (
	dateLayout = "2006-01-02"
	dueLayout  = "2006-01-02 15:04"
)

// clockOf 从一句话里取"一天中的具体时刻"（HH:MM）；第二个返回值表示是否明确写了时刻。
func clockOf(text string) (string, bool) {
	// 「今晚8点」「明早7点」里的今/明只影响日期，时刻按晚上/早上理解
	text = strings.NewReplacer("今晚", "晚上", "明晚", "晚上",
		"今早", "早上", "明早", "早上").Replace(text)
	if m := clockRe.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h <= 23 && mi <= 59 {
			return fmt.Sprintf("%02d:%02d", h, mi), true
		}
	}
	if m := hourRe.FindStringSubmatch(text); m != nil {
		h := numOf(m[2])
		mi := 0
		switch {
		case m[3] == "半":
			mi = 30
		case m[3] != "":
			mi = numOf(m[4])
		}
		switch m[1] {
		case "下午", "傍晚", "晚上", "夜里", "晚间":
			if h < 12 {
				h += 12
			}
		case "中午":
			h = 12
		}
		if h <= 23 && mi <= 59 {
			return fmt.Sprintf("%02d:%02d", h, mi), true
		}
	}
	return "09:00", false // 只说"哪天"：默认上午 9 点
}

// parseDueExpression 解析出提醒的完整时刻（YYYY-MM-DD HH:MM）。
// 只说"几点"不提日期时，取最近一次到来的那个时刻（今天已过则明天）。
func parseDueExpression(text string, now time.Time) (string, bool) {
	clock, explicit := clockOf(text)
	if d, ok := parseDateExpression(text, now); ok {
		return d + " " + clock, true
	}
	if !explicit || now.IsZero() {
		return "", false
	}
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	candidate, err := time.ParseInLocation(dueLayout, d.Format(dateLayout)+" "+clock, now.Location())
	if err != nil {
		return "", false
	}
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	if _, ok := saneDate(candidate, d); !ok {
		return "", false
	}
	return candidate.Format(dueLayout), true
}

// parseDateExpression 从一句话里解析出提醒日期（YYYY-MM-DD）；解析不出来返回空串。
// today 由宿主提供，函数本身不读墙钟。
func parseDateExpression(text string, today time.Time) (string, bool) {
	if today.IsZero() {
		return "", false
	}
	t := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	ok := func(d time.Time) (string, bool) { return saneDate(d, t) }

	if m := isoDateRe.FindStringSubmatch(text); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if s, fine := ok(time.Date(y, time.Month(mo), d, 0, 0, 0, 0, t.Location())); fine {
			return s, true
		}
	}
	switch {
	case strings.Contains(text, "大后天"):
		return ok(t.AddDate(0, 0, 3))
	case strings.Contains(text, "后天"):
		return ok(t.AddDate(0, 0, 2))
	case strings.Contains(text, "明天") || strings.Contains(text, "明日") ||
		strings.Contains(text, "明早") || strings.Contains(text, "明晚"):
		return ok(t.AddDate(0, 0, 1))
	case strings.Contains(text, "今天") || strings.Contains(text, "今日") || strings.Contains(text, "今儿") ||
		strings.Contains(text, "今晚") || strings.Contains(text, "今早") || strings.Contains(text, "今儿"):
		return ok(t)
	}
	if m := daysLaterRe.FindStringSubmatch(text); m != nil {
		return ok(t.AddDate(0, 0, numOf(m[1])))
	}
	if m := weeksLaterRe.FindStringSubmatch(text); m != nil {
		return ok(t.AddDate(0, 0, 7*numOf(m[1])))
	}
	if m := monthsLaterRe.FindStringSubmatch(text); m != nil {
		return ok(t.AddDate(0, numOf(m[1]), 0))
	}
	if m := weekRe.FindStringSubmatch(text); m != nil {
		monday := t.AddDate(0, 0, -int(t.Weekday()+6)%7) // 本周一
		w := weekdayOf(m[2])
		switch m[1] {
		case "下下":
			return ok(monday.AddDate(0, 0, 14+w-1))
		case "下":
			return ok(monday.AddDate(0, 0, 7+w-1))
		case "这", "本":
			return ok(monday.AddDate(0, 0, w-1))
		}
		d := monday.AddDate(0, 0, w-1)
		if d.Before(t) { // 「周三」在本周已经过了：指下一次
			d = d.AddDate(0, 0, 7)
		}
		return ok(d)
	}
	if m := nextMonthRe.FindStringSubmatch(text); m != nil {
		day, _ := strconv.Atoi(m[1])
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
		if s, fine := ok(first.AddDate(0, 0, day-1)); fine {
			return s, true
		}
	}
	if endOfMonth.MatchString(text) {
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
		return ok(first.AddDate(0, 0, -1))
	}
	if m := cnDateRe.FindStringSubmatch(text); m != nil {
		month, _ := strconv.Atoi(m[1])
		day, _ := strconv.Atoi(m[2])
		d := time.Date(t.Year(), time.Month(month), day, 0, 0, 0, 0, t.Location())
		if d.Month() != time.Month(month) || d.Day() != day { // 2 月 30 日这类
			return "", false
		}
		if d.Before(t) { // 说今年已过的日期 → 指明年
			d = d.AddDate(1, 0, 0)
		}
		return ok(d)
	}
	return "", false
}

// saneDate 过滤离谱日期：太远的未来（>5 年）当成解析失败，交给宿主回问，而不是写进库。
func saneDate(d, today time.Time) (string, bool) {
	if d.Before(today) || d.After(today.AddDate(5, 0, 0)) {
		return "", false
	}
	return d.Format(dateLayout), true
}

// normalizeDue 把输入规整成 YYYY-MM-DD HH:MM：
// 只给日期时补默认时刻 09:00；带时刻则保留。格式不合法返回空串。
func normalizeDue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) == len(dateLayout) {
		if t, err := time.Parse(dateLayout, s); err == nil {
			return t.Format(dateLayout) + " 09:00"
		}
		return ""
	}
	if t, err := time.Parse(dueLayout, s); err == nil {
		return t.Format(dueLayout)
	}
	return ""
}

// withinRange 判断时刻是否落在 [今天 00:00, 今天+5 年]。
func withinRange(s string, now time.Time) bool {
	d, err := time.Parse(dueLayout, s)
	if err != nil {
		return false
	}
	if now.IsZero() {
		return true
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return !d.Before(t) && !d.After(t.AddDate(5, 0, 0))
}

// resolveDue 决定提醒日期。三级兜底，保证「说要提醒」的这条内容不会因为算不出日期就消失：
//  1. 代码确定性解析（覆盖常见中文表述）
//  2. 模型给的绝对日期（校验格式与合理区间）
//  3. 都失败 → pending=true，由宿主回问用户「哪天？」；库不猜日期
func resolveDue(turn string, isReminder bool, modelDue string, at time.Time) (due string, pending bool) {
	if !isReminder {
		return "", false
	}
	if d, ok := parseDueExpression(turn, at); ok {
		return d, false
	}
	if m := normalizeDue(modelDue); m != "" && withinRange(m, at) {
		return m, false
	}
	return "", true
}
