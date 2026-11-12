package memory

import (
	"math"
	"strings"
)

// ReviewIntervals 复习间隔，单位是「经历轮次」而不是时间：
// 曲线形状沿用艾宾浩斯，自变量换成 tick（设计文档第二节「逻辑时钟」）。
var ReviewIntervals = []int{1, 2, 5, 12, 30, 70, 150, 300}

// ScheduleNextReview 按已复习次数取下一档间隔（越复习越久）。
func ScheduleNextReview(m *Memory) int {
	idx := m.ReviewCount
	if idx >= len(ReviewIntervals) {
		idx = len(ReviewIntervals) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return m.LastReviewTick + ReviewIntervals[idx]
}

// UpdateStrengthOnRecall 被召回视为一次被动复习，见设计文档第七节。
func UpdateStrengthOnRecall(m *Memory, relevance float64, tick int) {
	gain := 0.12 / (1 + float64(m.ReviewCount)*0.3)
	m.Strength = math.Min(1, m.Strength+gain*relevance)
	m.ReviewCount++
	m.LastReviewTick = tick
	m.DecayedTick = tick // 复习即重新开始计时
	m.NextReviewTick = ScheduleNextReview(m)
	m.Status = StatusActive
}

// ApplyDecay 按「经历了多少轮」结算衰减：越结实的记忆衰减越慢。
// 惰性结算——被访问或反思时才算，没有后台定时器，也不需要墙钟。
// 强度有地板：遗忘只降低清晰度（压缩），不会让记忆变成零——存储够用，就不该真的丢东西。
func ApplyDecay(m *Memory, tick int) {
	if m.Status == StatusArchived {
		return
	}
	if m.DueAt != "" || m.PendingDue {
		return // 提醒不被压缩：过不过期是日期判断，由 Reflect(today) 负责
	}
	delta := float64(tick - m.DecayedTick)
	if delta <= 0 {
		return
	}
	m.DecayedTick = tick
	rate := 0.02 / (1 + m.Strength*2 + m.Importance*3)
	decay := 1 - math.Exp(-rate*delta/DecayScale)
	m.Strength = math.Max(StrengthFloor, m.Strength-decay*m.Strength)

	if m.Strength < 0.05 && m.ReviewCount == 0 {
		m.Status = StatusArchived
	} else if m.Strength < 0.08 {
		m.Status = StatusFading
	}
}

// Freshness 新鲜度同样只看经历轮次，不看日期。
func Freshness(m *Memory, tick int) float64 {
	delta := float64(tick - m.LastReviewTick)
	if delta < 0 {
		delta = 0
	}
	return math.Exp(-delta / FreshScale)
}

var rememberKeywords = []string{"记住", "别忘了", "别忘", "remember"}

func hasRememberRequest(turn string) bool {
	lt := strings.ToLower(turn)
	for _, k := range rememberKeywords {
		if strings.Contains(lt, k) {
			return true
		}
	}
	return false
}

// ScoreImportance 纯规则重要性评分，见设计文档第六节。
// 只回答「先想起谁」，不回答「存不存」：这里没有丢弃。
func ScoreImportance(turn string, ex *Extracted, repeated bool) float64 {
	score := 0.0
	if hasRememberRequest(turn) {
		score += 0.5
	}
	if ex != nil {
		switch ex.Layer {
		case LayerIdentity:
			score += 0.3
		case LayerProcedural:
			score += 0.2
		}
		score += 0.1 * math.Min(float64(len(ex.Entities)), 3)
	}
	if repeated {
		score += 0.2
	}
	return math.Min(1, score)
}
