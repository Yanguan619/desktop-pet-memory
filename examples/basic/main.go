// 最小用法：一轮对话进来 → 编码成记忆 → 下次问话时召回。
// 运行：go run ./examples/basic   （需要本机 Ollama，或改成任意 OpenAI 兼容地址）
package main

import (
	"fmt"
	"log"
	"time"

	"desktop-pet-memory/memory"
)

func main() {
	store, err := memory.LoadStore("pet-memory.db") // 建库/校验 schema 版本
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

	turn := "记住：我偏好深色主题的界面"
	store.AppendTurn(turn)                                            // 先推进逻辑时钟并写工作记忆
	mems, err := eng.Encode(memory.Event{Turn: turn, At: time.Now()}) // At 只用于解析「明天」这类相对日期
	if err != nil {
		log.Fatal(err) // 模型不可用：本轮不产生长期记忆，由宿主决定怎么提示
	}
	for _, m := range mems {
		fmt.Printf("记住 [%s] %s\n", m.Layer, m.Content)
	}

	// 之后某次召回：入参是工作记忆，本轮在最后一条
	for _, m := range eng.Retrieve([]string{"界面配色我不喜欢亮的"}) {
		fmt.Printf("想起 [%s] %s\n", m.Layer, m.Content)
	}

	if err := store.Flush(); err != nil { // 落盘是宿主的责任
		log.Fatal(err)
	}
}
