package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const telegramBotToken = "8925815583:AAH5eVxUOLtKLiy50BQdkMX9Nb4wQgkD8bs"

// deliverOutput sends tick output to the configured delivery target.
// deliverFormat: "telegram:chat_id:thread_id" (from project deliver column).
// Falls back to the scheduler thread (83996) if no target is configured.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output to deliver", project, tickID)
		return
	}

	chatID := "-1003310984808"
	threadID := "83996" // default: scheduler foreman thread

	// Parse deliver target: "telegram:<chat_id>:<thread_id>"
	if deliver != "" {
		parts := strings.SplitN(deliver, ":", 3)
		if len(parts) >= 2 {
			chatID = parts[1]
		}
		if len(parts) >= 3 {
			threadID = parts[2]
		}
	}

	text := output.String()
	const maxLen = 4096
	if len(text) > maxLen {
		text = text[:maxLen-100] + "\n\n…[truncated]"
	}

	msg := fmt.Sprintf("🤖 Scheduler Tick: %s [%s]\n\n%s", project, tickID, text)

	body := map[string]any{
		"chat_id":           chatID,
		"message_thread_id": threadID,
		"text":              msg,
	}

	payload, _ := json.Marshal(body)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", telegramBotToken)

	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — Telegram POST failed: %v", project, tickID, err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != 200 {
		log.Printf("DELIVER: %s tick=%s — Telegram returned %d: %s", project, tickID, resp.StatusCode, strings.TrimSpace(string(respBody)))
		return
	}

	log.Printf("DELIVER: %s tick=%s → thread %s", project, tickID, threadID)
}
