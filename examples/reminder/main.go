// 提醒：日期由库解析成绝对时刻，宿主只负责定时轮询与展示。
// 运行：go run ./examples/reminder
package main

import (
	"fmt"
	"log"
	"time"

	"desktop-pet-memory/memory"
)

func main() {
	store, err := memory.LoadStore("pet-memory.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	model := memory.OpenAI{
		BaseURL:    "http://localhost:11434/v1",
		Model:      "qwen3.5:0.8b",
		EmbedModel: "qwen3-embedding:0.6b",
	}
	eng := memory.NewEngine(store, model, model, model)

	turn := "明天下午3点提醒我开会"
	store.AppendTurn(turn)
	mems, err := eng.Encode(memory.Event{Turn: turn, At: time.Now()}) // 「明天下午3点」在这里被锚定
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range mems {
		fmt.Printf("记下提醒 %s（%q）\n", m.Content, m.DueAt) // DueAt 为空说明没算出时刻
	}

	// 宿主定时器里做这三件事：
	now := time.Now()
	eng.Reflect(now) // 结算衰减；已交付且过期的提醒会被归档
	for _, m := range eng.DueReminders(now) {
		fmt.Printf("该提醒了：%s\n", m.Content) // 读出即标记已提醒，同一个提醒只响一次
	}
	for _, m := range eng.PendingReminders() {
		fmt.Printf("没说哪天：%s（用 ConfirmDue 补）\n", m.Content)
	}

	if err := store.Flush(); err != nil {
		log.Fatal(err)
	}
}
