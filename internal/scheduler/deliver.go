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

// deliverOutput sends tick output to the configured delivery target.
// The output buffer is non-truncated; deliverOutput trims to 4096 chars
// for Telegram's message limit and appends a truncation notice if needed.
func deliverOutput(project, tickID string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output to deliver", project, tickID)
		return
	}

	const telegramBotToken = "8925815583:AAH5eVxUOLtKLiy50BQdkMX9Nb4wQgkD8bs"
	const chatID = "-1003310984808"
	const threadID = "83996"

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

	log.Printf("DELIVER: %s tick=%s — delivered to Telegram thread %s", project, tickID, threadID)
}
