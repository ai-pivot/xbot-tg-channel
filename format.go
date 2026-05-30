package main

import (
	"fmt"
	"strings"

	tgmd "github.com/eekstunt/telegramify-markdown-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// convertMD converts markdown to Telegram plain text + entities using tgmd.
func convertMD(md string) (string, []tgbotapi.MessageEntity) {
	msgs := tgmd.Convert(md)
	entities := make([]tgbotapi.MessageEntity, len(msgs.Entities))
	for i, e := range msgs.Entities {
		entities[i] = tgbotapi.MessageEntity{
			Type:   string(e.Type),
			Offset: e.Offset,
			Length: e.Length,
		}
	}
	return msgs.Text, entities
}

// formatProgress formats a progress event into a display string.
// Stream content takes priority; then tool lines; then thinking/reasoning/phase.
func formatProgress(pe *progressEvent) string {
	// Stream content takes priority (it's the actual LLM output)
	if pe.StreamContent != "" {
		return pe.StreamContent
	}

	var b strings.Builder

	// Tool status line
	toolLine := formatToolLine(pe)
	if toolLine != "" {
		b.WriteString(toolLine)
		b.WriteByte('\n')
	}

	// Thinking/reasoning content (truncated)
	if pe.Thinking != "" {
		b.WriteString(truncate(strings.TrimSpace(pe.Thinking), 1500))
	} else if pe.Reasoning != "" {
		b.WriteString(truncate(strings.TrimSpace(pe.Reasoning), 1500))
	} else if pe.Phase != "done" && pe.Phase != "" {
		// No content yet, show phase
		switch pe.Phase {
		case "thinking":
			b.WriteString("💭 思考中...")
		case "tool_exec":
			// tool line already written above
		default:
			fmt.Fprintf(&b, "⏳ %s...", pe.Phase)
		}
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return ""
	}
	return result
}

// formatToolLine formats active and completed tool entries into a multi-line string.
func formatToolLine(pe *progressEvent) string {
	if len(pe.ActiveTools) == 0 && len(pe.CompletedTools) == 0 {
		return ""
	}

	var parts []string
	for _, t := range pe.ActiveTools {
		label := t.Label
		if label == "" {
			label = t.Name
		}
		icon := "⏳"
		switch t.Status {
		case "running":
			icon = "🔄"
		case "done":
			icon = "✅"
		case "error":
			icon = "❌"
		}
		parts = append(parts, fmt.Sprintf("%s %s", icon, label))
	}
	for _, t := range pe.CompletedTools {
		label := t.Label
		if label == "" {
			label = t.Name
		}
		entry := fmt.Sprintf("✅ %s", label)
		if t.Summary != "" {
			entry += " — " + truncate(t.Summary, 100)
		}
		parts = append(parts, entry)
	}

	return strings.Join(parts, "\n")
}

// truncate shortens a string to at most max bytes, appending "..." if truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
