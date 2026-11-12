package memory

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// reflect.go 反思线与提醒：睡眠整理、合簇、身份提炼、提醒交付与过期。
// 反思不是经历：tick 不因为反思而前进。

// Reflect 一次「睡眠整理」。
// now 由宿主提供，只用于"提醒过期"这一件事；零值表示不处理提醒。
func (e *Engine) Reflect(now time.Time) {
	tick := e.Store.Tick()
	for _, m := range e.Store.Active() {
		ApplyDecay(m, tick)
	}
	for _, m := range e.Store.All() {
		if m.Status == StatusFading && m.Strength < 0.03 {
			m.Status = StatusArchived
		}
		// 提醒过期且已经交付过：归档（原文保留，仍可召回），这就是「过期就归档」。
		// 没交付过的过期提醒不归档——否则宿主关机期间就是静默丢失。
		if !now.IsZero() && m.DueAt != "" && m.DueAt < now.Format(dueLayout) && m.Notified && m.Status != StatusArchived {
			m.Status = StatusArchived
		}
	}
	if e.MergeEnabled {
		e.mergeClusters(tick)
	}
	e.promoteIdentity(tick)
}

// DueReminders 返回该提醒的记忆，并标记为已通知（宿主负责展示与落盘）。
// 库不读墙钟：now 由宿主提供；接不上时间就不提醒，宁可晚一次也不早一次。
func (e *Engine) DueReminders(now time.Time) []*Memory {
	if now.IsZero() {
		return nil
	}
	nowKey := now.Format(dueLayout)
	var out []*Memory
	for _, m := range e.Store.All() {
		if m.DueAt == "" || m.Notified || m.PendingDue || m.Status == StatusArchived {
			continue
		}
		if m.DueAt <= nowKey {
			m.Notified = true
			out = append(out, m)
		}
	}
	return out
}

// PendingReminders 返回「用户说要提醒、但日期算不出来」的记忆，等宿主回问一句「哪天？」。
// 兜底的最后一环：算不准就绝不猜，但也绝不丢。
func (e *Engine) PendingReminders() []*Memory {
	var out []*Memory
	for _, m := range e.Store.All() {
		if m.PendingDue && m.Status != StatusArchived {
			out = append(out, m)
		}
	}
	return out
}

// ConfirmDue 由宿主在用户补上日期后调用：校验后写入，解除 pending。
func (e *Engine) ConfirmDue(id, due string, now time.Time) error {
	d := normalizeDue(due)
	if d == "" || !withinRange(d, now) {
		return fmt.Errorf("日期 %q 不合法（需要 YYYY-MM-DD，且在今天之后）", due)
	}
	for _, m := range e.Store.All() {
		if m.ID == id {
			m.DueAt, m.PendingDue, m.Notified = d, false, false
			return nil
		}
	}
	return fmt.Errorf("没有这条记忆: %s", id)
}

// mergeClusters embedding 聚类找候选，system2 判定是否同一件事并给出合并表述。
// 阈值是原始余弦（不是归一化分）：MergeThreshold 直接按实测分布定。
func (e *Engine) mergeClusters(tick int) {
	if e.Critic == nil || e.Embed == nil {
		return
	}
	active := e.Store.Active()
	used := make(map[string]bool, len(active))
	for i, a := range active {
		if used[a.ID] {
			continue
		}
		va := e.vectorOf(a)
		if va == nil {
			continue
		}
		cluster := []*Memory{a}
		for _, b := range active[i+1:] {
			if used[b.ID] || b.Layer != a.Layer {
				continue
			}
			if vb := e.vectorOf(b); vb != nil && Cosine(va, vb) >= MergeThreshold {
				cluster = append(cluster, b)
			}
		}
		if len(cluster) < 2 {
			continue
		}

		contents := make([]string, len(cluster))
		ids := make([]string, len(cluster))
		for k, m := range cluster {
			contents[k], ids[k] = m.Content, m.ID
		}
		merged, err := e.Critic.Merge(contents)
		if err != nil || merged == "" {
			continue // 判断不了/不该合并：保持原状
		}

		seed := cluster[0]
		importance, strength := seed.Importance, seed.Strength
		var tags []string
		for _, m := range cluster {
			used[m.ID] = true
			importance = math.Max(importance, m.Importance)
			strength = math.Max(strength, m.Strength)
			tags = union(tags, m.Tags)
			m.Status = StatusArchived // 原文保留
		}

		mem := &Memory{
			ID:             e.newID(),
			Layer:          seed.Layer,
			Content:        merged,
			Importance:     math.Min(1, importance),
			Strength:       math.Min(1, strength+0.05),
			CreatedTick:    tick,
			LastReviewTick: tick,
			DecayedTick:    tick,
			SourceEventID:  "reflect",
			Tags:           tags,
			Status:         StatusActive,
			Supersedes:     strings.Join(ids, ","),
		}
		mem.NextReviewTick = ScheduleNextReview(mem)
		e.attachVector(mem)
		e.Store.Upsert(mem)
	}
}

// promoteIdentity 同一标签的语义记忆复现足够多次，问 system2「这是不是稳定的用户画像」。
func (e *Engine) promoteIdentity(tick int) {
	if e.Critic == nil {
		return
	}
	byTag := map[string][]*Memory{}
	for _, m := range e.Store.Active() {
		if m.Layer != LayerSemantic {
			continue
		}
		for _, t := range m.Tags {
			byTag[t] = append(byTag[t], m)
		}
	}
	for tag, cluster := range byTag {
		if len(cluster) < IdentityMinEvidence || e.hasIdentityTag(tag) {
			continue
		}
		contents := make([]string, len(cluster))
		for k, m := range cluster {
			contents[k] = m.Content
		}
		stmt, err := e.Critic.Identity(contents)
		if err != nil || stmt == "" {
			continue
		}

		importance := 0.0
		for _, m := range cluster {
			importance = math.Max(importance, m.Importance)
		}
		mem := &Memory{
			ID:             e.newID(),
			Layer:          LayerIdentity,
			Content:        stmt,
			Importance:     math.Min(1, importance+0.2),
			Strength:       0.5,
			CreatedTick:    tick,
			LastReviewTick: tick,
			DecayedTick:    tick,
			SourceEventID:  "reflect",
			Tags:           []string{tag},
			Status:         StatusActive,
		}
		mem.NextReviewTick = ScheduleNextReview(mem)
		e.attachVector(mem)
		e.Store.Upsert(mem)
	}
}

func (e *Engine) hasIdentityTag(tag string) bool {
	for _, m := range e.Store.Active() {
		if m.Layer != LayerIdentity {
			continue
		}
		for _, t := range m.Tags {
			if t == tag {
				return true
			}
		}
	}
	return false
}

func union(a, b []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
