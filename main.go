// Telegram Channel Plugin for xbot.
// Feature-compatible with the built-in Feishu channel:
//   - Message patching (edit, not new per progress)
//   - AskUser inline keyboard panel
//   - Settings panel with LLM subscription management
//   - Typing indicator, dedup, user caching, permission filter
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	tgmd "github.com/eekstunt/telegramify-markdown-go"
)

// ─── Configuration ────────────────────────────────────────────────

type channelConfig struct {
	BotToken    string `json:"bot_token"`
	AllowFrom   string `json:"allow_from"`
	AllowGroups string `json:"allow_groups"`
	AdminChatID string `json:"admin_chat_id"`
}

// ─── Telegram Send / Edit (throttled ticker) ──────────────────────

// Per-chat pending edit state: latest text waiting to be displayed.
var (
	pendingText    = make(map[int64]string)
	pendingPlain   = make(map[int64]string)
	pendingEnts    = make(map[int64][]tgbotapi.MessageEntity)
	pendingMu      sync.Mutex
	editTicker     *time.Ticker
	editTickerDone chan struct{}
)

// startEditTicker launches a global goroutine that fires every 1s,
// checking all chats for pending updates and editing once per chat per tick.
func startEditTicker() {
	editTicker = time.NewTicker(1 * time.Second)
	editTickerDone = make(chan struct{})
	go func() {
		for {
			select {
			case <-editTicker.C:
				tickEditAll()
			case <-editTickerDone:
				editTicker.Stop()
				return
			}
		}
	}()
}

func stopEditTicker() {
	if editTickerDone != nil {
		close(editTickerDone)
	}
}

// tickEditAll checks all chats with pending edits and applies them.
func tickEditAll() {
	pendingMu.Lock()
	type entry struct {
		chatID   int64
		text     string
		plain    string
		entities []tgbotapi.MessageEntity
	}
	var entries []entry
	for chatID, text := range pendingText {
		entries = append(entries, entry{
			chatID:   chatID,
			text:     text,
			plain:    pendingPlain[chatID],
			entities: pendingEnts[chatID],
		})
		delete(pendingText, chatID)
		delete(pendingPlain, chatID)
		delete(pendingEnts, chatID)
	}
	pendingMu.Unlock()

	for _, e := range entries {
		doEditOrSend(e.chatID, e.text, e.plain, e.entities, false)
	}
}

// sendOrEdit queues a text update for throttled editing.
// forceNew sends immediately (bypasses throttle), used for user-facing messages.
func sendOrEdit(chatID int64, text string, forceNew bool) error {
	if bot == nil {
		return fmt.Errorf("bot not connected")
	}

	if strings.TrimSpace(text) == "💭" {
		text = "💭 思考中..."
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	plain, entities := convertMD(text)

	if forceNew {
		return doEditOrSend(chatID, text, plain, entities, true)
	}

	// Store pending — ticker will pick it up within 1s
	pendingMu.Lock()
	pendingText[chatID] = text
	pendingPlain[chatID] = plain
	pendingEnts[chatID] = entities
	pendingMu.Unlock()
	return nil
}

// flushPatch immediately sends any pending edit for a chat (for final messages).
func flushPatch(chatID int64) {
	pendingMu.Lock()
	text, ok := pendingText[chatID]
	if !ok {
		pendingMu.Unlock()
		return
	}
	plain := pendingPlain[chatID]
	ents := pendingEnts[chatID]
	delete(pendingText, chatID)
	delete(pendingPlain, chatID)
	delete(pendingEnts, chatID)
	pendingMu.Unlock()
	doEditOrSend(chatID, text, plain, ents, false)
}

// doEditOrSend is the actual send/edit logic.
// Handles long messages by splitting into multiple TG messages (4096 char limit).
func doEditOrSend(chatID int64, text string, plain string, entities []tgbotapi.MessageEntity, forceNew bool) error {
	// Split long messages at 4096 char boundary (on plain text length)
	if len(plain) > 4096 {
		return doSendLong(chatID, text, forceNew)
	}

	lastBotMsgIDMu.Lock()
	prevID, hasPrev := lastBotMsgID[chatID]
	lastBotMsgIDMu.Unlock()

	if hasPrev && !forceNew {
		lastTextMu.Lock()
		if text == lastText[chatID] {
			lastTextMu.Unlock()
			return nil
		}
		lastTextMu.Unlock()

		edit := tgbotapi.NewEditMessageText(chatID, prevID, plain)
		edit.Entities = entities
		_, err := bot.Send(edit)
		if err == nil {
			lastTextMu.Lock()
			lastText[chatID] = text
			lastTextMu.Unlock()
			return nil
		}
		errStr := err.Error()
		if strings.Contains(errStr, "message is not modified") {
			return nil
		}
		if isMessageGone(errStr) {
			logf("edit failed (message gone): %s", errStr)
		} else {
			if wait := parseRetryAfter(errStr); wait > 0 {
				logf("rate limited, waiting %v", wait)
				time.Sleep(wait)
				retry := tgbotapi.NewEditMessageText(chatID, prevID, plain)
				retry.Entities = entities
				if _, err := bot.Send(retry); err == nil {
					lastTextMu.Lock()
					lastText[chatID] = text
					lastTextMu.Unlock()
					return nil
				}
			} else {
				logf("edit failed: %s", errStr)
			}
			return nil
		}
	}

	cancelTyping(chatID)
	msg := tgbotapi.NewMessage(chatID, plain)
	msg.Entities = entities
	sent, err := bot.Send(msg)
	if err != nil {
		return err
	}
	lastBotMsgIDMu.Lock()
	lastBotMsgID[chatID] = sent.MessageID
	lastBotMsgIDMu.Unlock()
	lastTextMu.Lock()
	lastText[chatID] = text
	lastTextMu.Unlock()
	return nil
}

// doSendLong splits a long message into chunks and sends them.
// First chunk edits the existing message (if any), remaining chunks are sent as new messages.
func doSendLong(chatID int64, text string, forceNew bool) error {
	const chunkSize = 4000 // leave margin for entities/markdown expansion

	// Split original markdown text at chunk boundaries
	var chunks []string
	runes := []rune(text)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		} else {
			// Try to break at a newline for cleaner splits
			for j := end; j > end-200 && j > i; j-- {
				if runes[j] == '\n' {
					end = j + 1
					break
				}
			}
		}
		chunks = append(chunks, string(runes[i:end]))
		i = end - chunkSize // will be advanced by for loop
	}

	lastBotMsgIDMu.Lock()
	prevID, hasPrev := lastBotMsgID[chatID]
	lastBotMsgIDMu.Unlock()

	for i, chunk := range chunks {
		plain, entities := convertMD(chunk)
		if len(plain) > 4096 {
			plain = plain[:4090] + "..."
		}

		if i == 0 && hasPrev && !forceNew {
			// Edit existing message with first chunk
			edit := tgbotapi.NewEditMessageText(chatID, prevID, plain)
			edit.Entities = entities
			if _, err := bot.Send(edit); err != nil {
				logf("edit long msg failed: %s", err.Error())
				// Fallback to send new
				forceNew = true
			} else {
				lastTextMu.Lock()
				lastText[chatID] = chunk
				lastTextMu.Unlock()
				continue
			}
		}

		// Send as new message
		msg := tgbotapi.NewMessage(chatID, plain)
		msg.Entities = entities
		sent, err := bot.Send(msg)
		if err != nil {
			logf("send chunk %d failed: %s", i, err.Error())
			continue
		}
		lastBotMsgIDMu.Lock()
		lastBotMsgID[chatID] = sent.MessageID
		lastBotMsgIDMu.Unlock()
	}

	// Track full text so next edit knows the full content
	lastTextMu.Lock()
	lastText[chatID] = text
	lastTextMu.Unlock()
	return nil
}

func isMessageGone(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "message to edit not found") ||
		strings.Contains(lower, "bad request: message to edit not found") ||
		strings.Contains(lower, "message identifier is not specified")
}

func parseRetryAfter(errStr string) time.Duration {
	if !strings.Contains(errStr, "Too Many Requests") {
		return 0
	}
	if i := strings.Index(errStr, "retry after "); i >= 0 {
		rest := strings.TrimSpace(errStr[i+len("retry after "):])
		rest = strings.Split(rest, " ")[0]
		if sec, err := strconv.Atoi(rest); err == nil {
			return time.Duration(sec) * time.Second
		}
	}
	return time.Duration(15) * time.Second
}

// ─── Progress formatting (like Feishu card) ─────────────────────────// ─── Conversation History ─────────────────────────────────────────

type historyEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Sender  string `json:"sender,omitempty"`
	Time    string `json:"time"`
}

// ─── Global State ──────────────────────────────────────────────────

var (
	cfg          channelConfig
	bot          *tgbotapi.BotAPI
	stopCh       = make(chan struct{})
	shutdownOnce sync.Once

	allowSet    map[int64]bool
	allAllowed  bool
	allowGroups map[int64]bool
	allGroupsAllowed bool

	userNames   = make(map[int64]string)
	userNamesMu sync.RWMutex

	lastUpdateID int
	dedupMu      sync.Mutex

	history    = make(map[int64][]historyEntry)
	historyMu  sync.Mutex
	historyMax = 200

	rpcID   int64
	rpcIDMu sync.Mutex

	typingCancel = make(map[int64]chan struct{})
	typingMu     sync.Mutex

	// Message patching: chatID → last bot message ID
	lastBotMsgID   = make(map[int64]int)
	lastBotMsgIDMu sync.Mutex
	// Track last sent text per chat to skip no-change edits
	lastText   = make(map[int64]string)
	lastTextMu sync.Mutex

	// Callback data store: callback_id → handler data
	cbStore   = make(map[string]map[string]any)
	cbStoreMu sync.Mutex

	// RPC response channels: rpc_id → response channel
	rpcPending   = make(map[string]chan json.RawMessage)
	rpcPendingMu sync.Mutex

	// Per-chat busy lock: only one conversation at a time per chat
	chatBusy   = make(map[int64]bool)
	chatBusyMu sync.Mutex

	stdoutMu sync.Mutex
)

// ─── Logger ────────────────────────────────────────────────────────

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
}

func logf(format string, args ...any) {
	log.Printf("[tg] "+format, args...)
}

// ─── JSON-RPC Helpers ──────────────────────────────────────────────

func writeJSON(v any) {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	json.NewEncoder(os.Stdout).Encode(v)
}

func nextRPCID() string {
	rpcIDMu.Lock()
	rpcID++
	id := fmt.Sprintf("tg-%d", rpcID)
	rpcIDMu.Unlock()
	return id
}

func callRPC(method string, params map[string]any) (json.RawMessage, error) {
	id := nextRPCID()
	ch := make(chan json.RawMessage, 1)
	rpcPendingMu.Lock()
	rpcPending[id] = ch
	rpcPendingMu.Unlock()
	defer func() {
		rpcPendingMu.Lock()
		delete(rpcPending, id)
		rpcPendingMu.Unlock()
	}()

	writeJSON(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("RPC timeout: %s", method)
	}
}

func sendInbound(chatID int64, content string, senderID int64, senderName string, ctype string) {
	// New user message → clear patch tracking so the response starts fresh
	clearLastBotMsg(chatID)

	writeJSON(map[string]any{
		"id":     nextRPCID(),
		"method": "send_inbound",
		"params": map[string]any{
			"channel":     "tg",
			"chat_id":     fmt.Sprintf("tg:%d", chatID),
			"content":     content,
			"sender_id":   strconv.FormatInt(senderID, 10),
			"sender_name": senderName,
			"chat_type":   ctype,
		},
	})
}

func sendAskUserResponse(chatID int64, requestID string, answers map[string]string) {
	writeJSON(map[string]any{
		"id":     nextRPCID(),
		"method": "ask_user_response",
		"params": map[string]any{
			"channel":    "tg",
			"chat_id":    fmt.Sprintf("tg:%d", chatID),
			"request_id": requestID,
			"answers":    answers,
		},
	})
}

func sendRPCResponse(reqID string, result any) {
	writeJSON(map[string]any{"id": reqID, "result": result})
}

func sendRPCError(reqID string, errMsg string) {
	writeJSON(map[string]any{"id": reqID, "error": errMsg})
}

// ─── History ───────────────────────────────────────────────────────

func addHistory(chatID int64, role, content, sender string) {
	historyMu.Lock()
	defer historyMu.Unlock()
	h := history[chatID]
	h = append(h, historyEntry{Role: role, Content: content, Sender: sender, Time: time.Now().Format("15:04:05")})
	if len(h) > historyMax {
		h = h[len(h)-historyMax:]
	}
	history[chatID] = h
}

// ─── Typing ────────────────────────────────────────────────────────

func cancelTyping(chatID int64) {
	typingMu.Lock()
	if ch, ok := typingCancel[chatID]; ok {
		close(ch)
		delete(typingCancel, chatID)
	}
	typingMu.Unlock()
}

func startTyping(chatID int64) {
	cancelTyping(chatID)
	ch := make(chan struct{})
	typingMu.Lock()
	typingCancel[chatID] = ch
	typingMu.Unlock()
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		_ = sendChatAction(chatID, tgbotapi.ChatTyping)
		for {
			select {
			case <-ticker.C:
				_ = sendChatAction(chatID, tgbotapi.ChatTyping)
			case <-ch:
				return
			}
		}
	}()
}

func sendChatAction(chatID int64, action string) error {
	if bot == nil {
		return fmt.Errorf("bot not connected")
	}
	_, err := bot.Request(tgbotapi.NewChatAction(chatID, action))
	return err
}


// ─── Progress formatting (like Feishu card) ─────────────────────────

func formatProgress(progress map[string]any) string {
	phase, _ := progress["phase"].(string)
	thinking, _ := progress["thinking"].(string)
	reasoning, _ := progress["reasoning"].(string)
	sc, _ := progress["stream_content"].(string)

	var b strings.Builder

	// Stream content takes priority (it's the actual LLM output)
	if sc != "" {
		return sc
	}

	// Tool status line
	toolLine := formatToolLine(progress)
	if toolLine != "" {
		b.WriteString(toolLine)
		b.WriteByte('\n')
	}

	// Thinking/reasoning content (truncated)
	if thinking != "" {
		b.WriteString(truncate(strings.TrimSpace(thinking), 1500))
	} else if reasoning != "" {
		b.WriteString(truncate(strings.TrimSpace(reasoning), 1500))
	} else if phase != "done" && phase != "" {
		// No content yet, show phase
		switch phase {
		case "thinking":
			b.WriteString("💭 思考中...")
		case "tool_exec":
			// tool line already written above
		default:
			fmt.Fprintf(&b, "⏳ %s...", phase)
		}
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return ""
	}
	return result
}

func formatToolLine(progress map[string]any) string {
	var active, completed []map[string]any

	if at, ok := progress["active_tools"].([]any); ok {
		for _, t := range at {
			if m, ok := t.(map[string]any); ok {
				active = append(active, m)
			}
		}
	}
	if ct, ok := progress["completed_tools"].([]any); ok {
		for _, t := range ct {
			if m, ok := t.(map[string]any); ok {
				completed = append(completed, m)
			}
		}
	}

	if len(active) == 0 && len(completed) == 0 {
		return ""
	}

	var parts []string
	for _, t := range active {
		label := t["label"]
		if label == nil {
			name, _ := t["name"].(string)
			label = name
		}
		status, _ := t["status"].(string)
		icon := "⏳"
		switch status {
		case "running":
			icon = "🔄"
		case "done":
			icon = "✅"
		case "error":
			icon = "❌"
		}
		parts = append(parts, fmt.Sprintf("%s %s", icon, label))
	}
	for _, t := range completed {
		label := t["label"]
		if label == nil {
			name, _ := t["name"].(string)
			label = name
		}
		summary, _ := t["summary"].(string)
		entry := fmt.Sprintf("✅ %s", label)
		if summary != "" {
			entry += " — " + truncate(summary, 100)
		}
		parts = append(parts, entry)
	}

	return strings.Join(parts, "\n")
}

// ─── Markdown conversion (via tgmd entities) ────────────────────

// convertMD converts markdown to Telegram plain text + entities using tgmd.
// Returns text, entities slice.
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

// applyEntities sets entities on a message config from convertMD result.
func applyEntities(msgConfig *tgbotapi.MessageConfig, text string) {
	_, entities := convertMD(text)
	msgConfig.Entities = entities
}

// applyEntitiesEdit sets entities on an edit config from convertMD result.
func applyEntitiesEdit(editConfig *tgbotapi.EditMessageTextConfig, text string) {
	_, entities := convertMD(text)
	editConfig.Entities = entities
}

func sendMessageWithKeyboard(chatID int64, text string, keyboard [][]tgbotapi.InlineKeyboardButton) (int, error) {
	if bot == nil {
		return 0, fmt.Errorf("bot not connected")
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	sent, err := bot.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

func clearLastBotMsg(chatID int64) {
	lastBotMsgIDMu.Lock()
	delete(lastBotMsgID, chatID)
	lastBotMsgIDMu.Unlock()
	lastTextMu.Lock()
	delete(lastText, chatID)
	lastTextMu.Unlock()
}

// ─── User Name ──────────────────────────────────────────────────────

func getUserName(user *tgbotapi.User) string {
	if user == nil {
		return "Unknown"
	}
	userNamesMu.RLock()
	name, ok := userNames[user.ID]
	userNamesMu.RUnlock()
	if ok {
		return name
	}
	name = user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if name == "" {
		name = user.UserName
	}
	if name == "" {
		name = fmt.Sprintf("User%d", user.ID)
	}
	userNamesMu.Lock()
	userNames[user.ID] = name
	userNamesMu.Unlock()
	return name
}

// ─── Permission ─────────────────────────────────────────────────────

func isAllowed(userID int64) bool {
	if allAllowed {
		return true
	}
	return allowSet[userID]
}

func parseAllowList(raw string) {
	allowSet = make(map[int64]bool)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		allAllowed = true
		return
	}
	allAllowed = false
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			allowSet[id] = true
		}
	}
}

func parseAllowGroups(raw string) {
	allowGroups = make(map[int64]bool)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		allGroupsAllowed = true
		return
	}
	allGroupsAllowed = false
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			allowGroups[id] = true
		}
	}
}

func isGroupAllowed(groupID int64) bool {
	if allGroupsAllowed {
		return true
	}
	return allowGroups[groupID]
}

func chatTypeStr(chat *tgbotapi.Chat) string {
	if chat.IsGroup() || chat.IsSuperGroup() {
		return "group"
	}
	return "p2p"
}

// ─── Telegram Update Handling ──────────────────────────────────────

func isDuplicate(updateID int) bool {
	dedupMu.Lock()
	defer dedupMu.Unlock()
	if updateID <= lastUpdateID {
		return true
	}
	lastUpdateID = updateID
	return false
}

func onUpdate(update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		onCallbackQuery(update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}
	msg := update.Message
	chat := msg.Chat
	chatID := chat.ID
	user := msg.From

	if isDuplicate(update.UpdateID) {
		return
	}

	// Commands
	// Log every incoming TG message for debugging
	logf("TG msg: chatID=%d chatType=%s chatTitle=%q userID=%d text=%q",
		chatID, chatTypeStr(chat), chat.Title,
		func() int64 { if user != nil { return user.ID }; return 0 }(),
		truncate(msg.Text, 50))

		if msg.IsCommand() {
		onCommand(chatID, user, msg.Command())
		return
	}

	if user != nil && user.IsBot {
		return
	}

	// Permission: group check (by chat ID), user check (by user ID)
	if chat.IsGroup() || chat.IsSuperGroup() {
		if !isGroupAllowed(chatID) {
			logf("access denied: group %d (%s)", chatID, chat.Title)
			return
		}
		// In groups, only respond when @mentioned or replied to
		if !isMentionedOrReply(msg) {
			return
		}
	} else {
		if user != nil && !isAllowed(user.ID) {
			logf("access denied for user %d (%s)", user.ID, getUserName(user))
			return
		}
	}

content := msg.Text
 if content == "" {
  content = msg.Caption
 }
 if content == "" {
  return
 }

 senderID := int64(0)
 senderName := "Unknown"
 if user != nil {
  senderID = user.ID
  senderName = getUserName(user)
 }

 ctype := chatTypeStr(chat)
 logf("msg from %s (%d) in %s %d: %s", senderName, senderID, ctype, chatID, truncate(content, 60))
 addHistory(chatID, "user", content, senderName)

 // Queue: skip if chat is busy
 chatBusyMu.Lock()
 if chatBusy[chatID] {
  chatBusyMu.Unlock()
  sendOrEdit(chatID, "⏳ 上一个请求还在处理中，请稍后再试...\n/cancel 取消当前请求", true)
  return
 }
 chatBusy[chatID] = true
 chatBusyMu.Unlock()

 clearLastBotMsg(chatID)
 sendInbound(chatID, content, senderID, senderName, ctype)
}

// isMentionedOrReply checks if the bot is @mentioned or was replied to.
func isMentionedOrReply(msg *tgbotapi.Message) bool {
	if bot == nil {
		return false
	}
	// Reply to bot's message
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.ID == bot.Self.ID {
		return true
	}
	// @mention in text
	if strings.Contains(msg.Text, "@"+bot.Self.UserName) {
		return true
	}
	return false
}

// ─── Callback Query Handling ───────────────────────────────────────

func onCallbackQuery(cb *tgbotapi.CallbackQuery) {
	if cb == nil || cb.Data == "" {
		return
	}
	data := cb.Data
	chatID := cb.Message.Chat.ID

	// Answer the callback to stop loading animation
	bot.Request(tgbotapi.NewCallback(cb.ID, ""))

if strings.HasPrefix(data, "ask:") {
  handleAskCallback(chatID, cb.Message.MessageID, data)
 } else if strings.HasPrefix(data, "set:") {
  handleSettingsCallback(chatID, cb.Message.MessageID, data)
 } else if strings.HasPrefix(data, "cmd:") {
  cmd := strings.TrimPrefix(data, "cmd:")
  handleCommandCallback(chatID, cb.Message.MessageID, cmd)
 } else if data == "settings_main" {
  handleSettingsMain(chatID, cb.Message.MessageID)
 }
}

func handleCommandCallback(chatID int64, msgID int, cmd string) {
 switch cmd {
 case "new":
  handleNew(chatID)
 case "cancel":
  handleCancel(chatID)
 case "settings":
  handleSettingsMain(chatID, msgID)
 case "history":
  handleHistory(chatID)
 }
}

func handleAskCallback(chatID int64, msgID int, data string) {
	// Format: "ask:requestID:qIndex:answer"
	parts := strings.SplitN(data, ":", 4)
	if len(parts) < 4 {
		return
	}
	requestID := parts[1]
	answer := parts[3]

	// Remove the inline keyboard
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, tgbotapi.InlineKeyboardMarkup{})
	bot.Send(edit)

	sendAskUserResponse(chatID, requestID, map[string]string{"0": answer})
	logf("ask_user answered: request=%s answer=%s", requestID, truncate(answer, 50))
}

// ─── Chat busy queue ────────────────────────────────────────────────

func releaseBusy(chatID int64) {
	chatBusyMu.Lock()
	delete(chatBusy, chatID)
	chatBusyMu.Unlock()
	logf("chat %d released from busy", chatID)
}

func isBusy(chatID int64) bool {
	chatBusyMu.Lock()
	b := chatBusy[chatID]
	chatBusyMu.Unlock()
	return b
}

// ─── Commands ──────────────────────────────────────────────────────

func onCommand(chatID int64, user *tgbotapi.User, cmd string) {
	if !isAllowed(user.ID) {
		return
	}

	switch cmd {
	case "start", "help":
		handleStart(chatID)
	case "cancel":
		handleCancel(chatID)
	case "settings":
		handleSettingsMain(chatID, 0)
	case "history":
		handleHistory(chatID)
	case "new":
		handleNew(chatID)
	}
}

func handleStart(chatID int64) {
	text := "👋 Hello! I'm xbot via Telegram.\n\nSend me a message and I'll respond.\n\nUse the buttons below or type a command:"
	keyboard := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("💬 New Conversation", "cmd:new")},
		{tgbotapi.NewInlineKeyboardButtonData("❌ Cancel Current", "cmd:cancel")},
		{tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "cmd:settings")},
		{tgbotapi.NewInlineKeyboardButtonData("📜 History", "cmd:history")},
	}
	sendMessageWithKeyboard(chatID, text, keyboard)
}

func handleCancel(chatID int64) {
	if !isBusy(chatID) {
		sendOrEdit(chatID, "✅ 没有正在处理的请求。", true)
		return
	}
	releaseBusy(chatID)
	clearLastBotMsg(chatID)
	cancelTyping(chatID)
	sendOrEdit(chatID, "❌ 请求已取消。", true)
	// Tell xbot to cancel
	sendInbound(chatID, "/cancel", 0, "", "")
}

func handleNew(chatID int64) {
	releaseBusy(chatID)
	clearLastBotMsg(chatID)
	sendOrEdit(chatID, "✅ 新对话已开始，请发送消息。", true)
	sendInbound(chatID, "/new", 0, "", "")
}

func handleHistory(chatID int64) {
	historyMu.Lock()
	h := history[chatID]
	historyMu.Unlock()
	if len(h) == 0 {
		sendOrEdit(chatID, "_No conversation history yet._", true)
		return
	}
	var sb strings.Builder
	for _, e := range h {
		sb.WriteString(fmt.Sprintf("*%s*: %s\n", e.Role, truncate(e.Content, 200)))
		if sb.Len() > 3500 {
			sb.WriteString("...\n")
			break
		}
	}
	sendOrEdit(chatID, sb.String(), true)
}

// ─── Settings Panel (LLM Configuration) ────────────────────────────

func handleSettingsMain(chatID int64, msgID int) {
	text := "⚙️ *Settings*\n\nSelect an option:"
	keyboard := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("📋 List Subscriptions", "set:list")},
		{tgbotapi.NewInlineKeyboardButtonData("⚡ Set LLM Model", "set:model")},
		{tgbotapi.NewInlineKeyboardButtonData("🔐 Set API Key", "set:apikey")},
		{tgbotapi.NewInlineKeyboardButtonData("🔗 Set Base URL", "set:baseurl")},
	}
	if msgID != 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = tgbotapi.ModeMarkdown
		edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
		bot.Send(edit)
	} else {
		sendMessageWithKeyboard(chatID, text, keyboard)
	}
}

func handleSettingsCallback(chatID int64, msgID int, data string) {
	switch data {
	case "set:list":
		handleSettingsList(chatID, msgID)
	case "set:model":
		// Prompt for model name - wait for user text input
		edit := tgbotapi.NewEditMessageText(chatID, msgID, "Please send me the model name (e.g. `deepseek-chat`, `gpt-4o`):")
		edit.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(edit)
		storeCallback(chatID, "set_model")
	case "set:apikey":
		edit := tgbotapi.NewEditMessageText(chatID, msgID, "Please send me the API key:")
		edit.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(edit)
		storeCallback(chatID, "set_apikey")
	case "set:baseurl":
		edit := tgbotapi.NewEditMessageText(chatID, msgID, "Please send me the Base URL (e.g. `https://api.deepseek.com/v1`):")
		edit.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(edit)
		storeCallback(chatID, "set_baseurl")
	default:
		handleSettingsMain(chatID, msgID)
	}
}

func handleSettingsList(chatID int64, msgID int) {
	// Call list_subscriptions RPC
	resp, err := callRPC("list_subscriptions", map[string]any{})
	if err != nil {
		editText(chatID, msgID, fmt.Sprintf("Error fetching subscriptions: %v", err))
		return
	}

	var result struct {
		Subs []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Active   bool   `json:"active"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		editText(chatID, msgID, fmt.Sprintf("Parse error: %v", err))
		return
	}

	var sb strings.Builder
	sb.WriteString("*📋 Subscriptions*\n\n")
	for _, s := range result.Subs {
		active := ""
		if s.Active {
			active = " ✅"
		}
		sb.WriteString(fmt.Sprintf("• *%s* — `%s` @ `%s`%s\n", s.Name, s.Model, s.Provider, active))
	}
	if len(result.Subs) == 0 {
		sb.WriteString("_No subscriptions configured._\n")
	}

	keyboard := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Settings", "settings_main")},
	}
	if msgID != 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, sb.String())
		edit.ParseMode = tgbotapi.ModeMarkdown
		edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
		bot.Send(edit)
	} else {
		sendMessageWithKeyboard(chatID, sb.String(), keyboard)
	}
}

func editText(chatID int64, msgID int, text string) {
	if msgID != 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(edit)
	} else {
		sendOrEdit(chatID, text, true)
	}
}

// ─── Callback Store for multi-step settings ────────────────────────

type cbState struct {
	Action string
	ChatID int64
}

var (
	cbStates   = make(map[int64]*cbState)
	cbStatesMu sync.Mutex
)

func storeCallback(chatID int64, action string) {
	cbStatesMu.Lock()
	cbStates[chatID] = &cbState{Action: action, ChatID: chatID}
	cbStatesMu.Unlock()
}

func getCallback(chatID int64) *cbState {
	cbStatesMu.Lock()
	defer cbStatesMu.Unlock()
	s := cbStates[chatID]
	delete(cbStates, chatID)
	return s
}

// ─── AskUser Panel ─────────────────────────────────────────────────

func handleAskUser(chatID int64, questions []map[string]any, requestID string) {
	if len(questions) == 0 {
		return
	}

	q := questions[0]
	question, _ := q["question"].(string)
	opts, _ := q["options"].([]any)

	var sb strings.Builder
	sb.WriteString("❓ *")
	sb.WriteString(question)
	sb.WriteString("*")

	if len(opts) > 0 {
		// Build inline keyboard
		var buttons [][]tgbotapi.InlineKeyboardButton
		for _, o := range opts {
			optStr := fmt.Sprintf("%v", o)
			data := fmt.Sprintf("ask:%s:0:%s", requestID, optStr)
			buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(optStr, data),
			})
		}
		_, err := sendMessageWithKeyboard(chatID, sb.String(), buttons)
		if err != nil {
			logf("ask_user send failed: %v", err)
		}
	} else {
		// Free text question
		sendOrEdit(chatID, sb.String()+"\n\n_Please reply with your answer._", true)
		storeCallback(chatID, fmt.Sprintf("ask:%s:0", requestID))
	}
}

// ─── Stdio Protocol Handling ───────────────────────────────────────

func handleActivate() map[string]any {
	return map[string]any{
		"channel_provider": map[string]any{
			"name": "tg",
			"config_schema": []map[string]any{
				{"key": "enabled", "label": "Enable", "type": "toggle", "default_value": "false", "category": "Telegram"},
				{"key": "bot_token", "label": "Bot Token", "description": "From @BotFather", "type": "password", "category": "Telegram"},
				{"key": "allow_from", "label": "Allowed Users", "description": "Comma-separated user IDs (empty=all)", "type": "text", "category": "Telegram"},
				{"key": "allow_groups", "label": "Allowed Groups", "description": "Comma-separated group IDs (empty=all)", "type": "text", "category": "Telegram"},
				{"key": "admin_chat_id", "label": "Admin Chat ID", "description": "Optional", "type": "text", "category": "Telegram"},
			},
		},
	}
}

func handleIncoming(line string) {
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return
	}

	msgID, _ := msg["id"].(string)
	msgType, _ := msg["type"].(string)
	msgMethod, _ := msg["method"].(string)

	// 1. RPC response from xbot → deliver to pending call
	if msgID != "" && msgMethod == "" && msgType == "" {
		data, _ := json.Marshal(msg)
		rpcPendingMu.Lock()
		ch, ok := rpcPending[msgID]
		rpcPendingMu.Unlock()
		if ok {
			select {
			case ch <- data:
			default:
			}
		}
		return
	}

	// 2. Old-style plugin request (method, no id)
	if msgMethod != "" && msgID == "" {
		handlePluginRequest(msg)
		return
	}

	// 3. RPC request from xbot (id + method)
	if msgID != "" && msgMethod != "" {
		handleXbotRPC(msgID, msgMethod, msg)
		return
	}

	// 4. Event push from xbot (type)
	if msgType != "" {
		handleXbotEvent(msg)
	}
}

func handlePluginRequest(msg map[string]any) {
	method, _ := msg["method"].(string)
	switch method {
	case "activate":
		resp := handleActivate()
		resp["result"] = "ok"
		writeJSON(resp)
	default:
		writeJSON(map[string]any{"error": fmt.Sprintf("unknown method: %s", method)})
	}
}

func handleXbotRPC(reqID, method string, msg map[string]any) {
	// New transport pushes events (text with is_final=true), not RPC calls.
	// channel_send is legacy from old grpcChannelBridge — ignore it.
	sendRPCResponse(reqID, "ok")
}

func parseChatID(s string) int64 {
	s = strings.TrimPrefix(s, "tg:")
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

func handleXbotEvent(msg map[string]any) {
	msgType, _ := msg["type"].(string)
	content, _ := msg["content"].(string)
	chatIDStr, _ := msg["chat_id"].(string)
	chatID := parseChatID(chatIDStr)

	// Debug: log every event to stderr
	logf("EVENT type=%q chat_id=%q content_len=%d has_progress=%v", msgType, chatIDStr, len(content), msg["progress"] != nil)
	if msg["progress"] != nil {
		pData, _ := json.Marshal(msg["progress"])
		logf("EVENT progress=%s", string(pData))
	}

	switch msgType {
	case "channel_config":
		if meta, _ := msg["metadata"].(map[string]any); meta != nil {
			if cfgRaw, ok := meta["config"]; ok {
				if cfgStr, ok := cfgRaw.(string); ok {
					json.Unmarshal([]byte(cfgStr), &cfg)
				}
			}
		}
		logf("config: allow=%q groups=%q admin=%q", cfg.AllowFrom, cfg.AllowGroups, cfg.AdminChatID)
		parseAllowList(cfg.AllowFrom)
		parseAllowGroups(cfg.AllowGroups)
		startBot()

	case "session":
		// ignore

	default:
		// ALL other events (text, stream_content, progress_structured) edit ONE message
		if chatID == 0 {
			break
		}
		startTyping(chatID)

		var display string
		isDone := false

		// Final text message (from agent's Send)
		if msgType == "text" && content != "" {
			display = content
		} else if progress, ok := msg["progress"].(map[string]any); ok {
			phase, _ := progress["phase"].(string)
			if phase == "done" {
				isDone = true
			}
			// Format full progress view (like Feishu card)
			display = formatProgress(progress)
		}

		if display == "" {
			display = "💭 思考中..."
		}

		// Check AskUser
		if progress, ok := msg["progress"].(map[string]any); ok {
			if questions, ok := progress["questions"].([]any); ok && len(questions) > 0 {
				converted := make([]map[string]any, len(questions))
				for i, q := range questions {
					converted[i], _ = q.(map[string]any)
				}
				requestID, _ := progress["request_id"].(string)
				handleAskUser(chatID, converted, requestID)
			}
		}

		isFinal := msgType == "text" && isFinal(msg)

		// phase:done with thinking = final reply from LLM
		if isDone && display != "" && display != "💭 思考中..." {
			_ = sendOrEdit(chatID, display, false)
			addHistory(chatID, "assistant", display, "")
			cancelTyping(chatID)
		} else if isFinal && content != "" {
			_ = sendOrEdit(chatID, content, false)
		} else {
			_ = sendOrEdit(chatID, display, false)
		}

		// Final text event → flush pending, add to history, release busy lock
		if isFinal {
			flushPatch(chatID)
			cancelTyping(chatID)
			if content != "" {
				addHistory(chatID, "assistant", content, "")
			}
			releaseBusy(chatID)
		}
		// phase:done also flush and release
		if isDone && display != "" && display != "💭 思考中..." {
			flushPatch(chatID)
			releaseBusy(chatID)
		}
	}
}

func isFinal(msg map[string]any) bool {
	meta, _ := msg["metadata"].(map[string]any)
	if meta == nil {
		return false
	}
	v, _ := meta["is_final"].(string)
	return v == "true"
}

// ─── Bot Lifecycle ─────────────────────────────────────────────────

func startBot() {
	if cfg.BotToken == "" {
		logf("no bot_token, waiting...")
		return
	}
	var err error
	bot, err = tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		logf("bot create failed: %v", err)
		return
	}
	bot.Debug = false
	logf("bot connected as @%s", bot.Self.UserName)
	// Ensure webhook is removed so polling works cleanly
	bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true})
	// Start throttled edit ticker (1 edit per second per chat max)
	startEditTicker()
	go pollingLoop()
}

func pollingLoop() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		updates, err := bot.GetUpdates(u)
		if err != nil {
			logf("getUpdates error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, update := range updates {
			onUpdate(update)
			u.Offset = update.UpdateID + 1
		}
	}
}

// ─── Main ──────────────────────────────────────────────────────────

func main() {
	logf("starting Telegram channel plugin")
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			logf("read error: %v", err)
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		handleIncoming(line)
	}
	shutdownOnce.Do(func() { close(stopCh) })
	logf("stdin closed, shutting down")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
