package memory

import (
	"testing"
	"time"
)

// moment 构造"本地时刻"，配合 DueAt 的 YYYY-MM-DD HH:MM 比较。
func moment(s string) time.Time {
	t, err := time.ParseInLocation(dueLayout, s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// 2026-11-12 是周四；所有用例都以它为"今天"，不读墙钟。
func day(s string) time.Time {
	t, err := time.ParseInLocation(dateLayout, s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseDateExpression(t *testing.T) {
	today := day("2026-11-12") // 周四
	cases := []struct{ text, want string }{
		{"2026-11-15 提醒我买蛋糕", "2026-11-15"},
		{"明天提醒我给妈妈打电话", "2026-11-13"},
		{"后天交房租", "2026-11-14"},
		{"大后天取快递", "2026-11-15"},
		{"今天买牛奶", "2026-11-12"},
		{"3 天后提醒我复查", "2026-11-15"},
		{"两周后提醒我复诊", "2026-11-26"},
		{"两个月后提醒我续保", "2027-01-12"},
		{"下周三提醒我交房租", "2026-11-18"},
		{"周三提醒我交房租", "2026-11-18"}, // 本周三已过 → 下一次
		{"周五提醒我开会", "2026-11-13"},  // 本周五还没到
		{"下下周一提醒我开会", "2026-11-23"},
		{"这周日提醒我休息", "2026-11-15"},
		{"下个月5号提醒我缴费", "2026-12-05"},
		{"月底提醒我结账", "2026-11-30"},
		{"11月20日提醒我体检", "2026-11-20"},
		{"1月5号提醒我续费", "2027-01-05"}, // 今年已过 → 明年
		{"2026-02-30 提醒我", ""},      // 不存在的日期
		{"我们聊聊记忆系统", ""},
	}
	for _, c := range cases {
		got, ok := parseDateExpression(c.text, today)
		if c.want == "" {
			if ok {
				t.Errorf("parseDateExpression(%q) = %q, 期望解析不出来", c.text, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("parseDateExpression(%q) = %q(%v), want %q", c.text, got, ok, c.want)
		}
	}
}

// 太远/过去的日期不接受：宁可让宿主回问，也不要写一个离谱日期。
func TestParseDateRejectsAbsurd(t *testing.T) {
	today := day("2026-11-12")
	for _, text := range []string{"2035年之前提醒我", "2026-11-01 提醒我"} {
		if got, ok := parseDateExpression(text, today); ok {
			t.Errorf("%q 不该解析成 %q", text, got)
		}
	}
}

// 时刻解析：日期 + 一天中的具体时间；只说日期时默认 09:00。
func TestParseDueExpressionTime(t *testing.T) {
	today := day("2026-11-12")
	cases := []struct{ text, want string }{
		{"明天下午3点提醒我开会", "2026-11-13 15:00"},
		{"明天下午3点半提醒我开会", "2026-11-13 15:30"},
		{"明天 15:30 提醒我开会", "2026-11-13 15:30"},
		{"明早8点提醒我吃药", "2026-11-13 08:00"},
		{"明天早上7点20分提醒我出门", "2026-11-13 07:20"},
		{"今晚8点提醒我追剧", "2026-11-12 20:00"},
		{"中午12点提醒我吃饭", "2026-11-12 12:00"},
		{"凌晨2点提醒我看球", "2026-11-12 02:00"},
		{"11月20日 09:15 提醒我体检", "2026-11-20 09:15"},
		{"11月20日提醒我体检", "2026-11-20 09:00"}, // 没说时刻
		{"下周三提醒我交房租", "2026-11-18 09:00"},
	}
	for _, c := range cases {
		got, ok := parseDueExpression(c.text, today)
		if !ok || got != c.want {
			t.Errorf("parseDueExpression(%q) = %q(%v), want %q", c.text, got, ok, c.want)
		}
	}
}

// 三级兜底：代码算出来优先，其次模型绝对日期，都不行就 pending（等宿主回问）。
func TestResolveDueThreeLevels(t *testing.T) {
	today := day("2026-11-12")

	if due, pending := resolveDue("下周三提醒我交房租", true, "2026-11-13", today); due != "2026-11-18 09:00" || pending {
		t.Errorf("应按原文重算, got %q pending=%v", due, pending)
	}
	if due, pending := resolveDue("提醒我处理那件事", true, "2026-12-25", today); due != "2026-12-25 09:00" || pending {
		t.Errorf("算不出来时采用模型日期, got %q pending=%v", due, pending)
	}
	if _, pending := resolveDue("有空提醒我把书还了", true, "", today); !pending {
		t.Error("算不出来且模型也没给 → 应该 pending，交宿主回问")
	}
	if _, pending := resolveDue("有空提醒我把书还了", true, "下周", today); !pending {
		t.Error("模型给了非法日期 → 应该 pending")
	}
	if _, pending := resolveDue("有空提醒我把书还了", true, "2020-01-01", today); !pending {
		t.Error("模型给了过去的日期 → 应该 pending")
	}
	if due, pending := resolveDue("我们在 2026-11-15 讨论过这个方案", false, "", today); due != "" || pending {
		t.Errorf("不是提醒就不该有 due, got %q pending=%v", due, pending)
	}
}
