package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ─── Global State ─────────────────────────────────────────────────────

var (
	cfg          channelConfig
	bot          *tgbotapi.BotAPI
	stopCh       = make(chan struct{})
	shutdownOnce sync.Once

	allowSet         map[int64]bool
	allAllowed       bool
	allowGroups      map[int64]bool
	allGroupsAllowed bool

	userNames   = make(map[int64]string)
	userNamesMu sync.RWMutex

	lastUpdateID int
	dedupMu      sync.Mutex

	history    = make(map[int64][]historyEntry)
	historyMu  sync.Mutex
	historyMax = 200

	typingCancel = make(map[int64]chan struct{})
	typingMu     sync.Mutex

	// Message patching: chatID → last bot message ID
	lastBotMsgID   = make(map[int64]int)
	lastBotMsgIDMu sync.Mutex
	// Track last user message ID per chat (for react emoji)
	lastUserMsgID   = make(map[int64]int)
	lastUserMsgIDMu sync.Mutex
	// Track last sent text per chat to skip no-change edits
	lastText   = make(map[int64]string)
	lastTextMu sync.Mutex

	// Callback data store for multi-step settings
	cbStates   = make(map[int64]*cbState)
	cbStatesMu sync.Mutex

	// Per-chat busy lock: only one conversation at a time per chat
	chatBusy   = make(map[int64]bool)
	chatBusyMu sync.Mutex

	stdoutMu sync.Mutex
)

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
}

// ─── Logger ───────────────────────────────────────────────────────────

func logf(format string, args ...any) {
	log.Printf("[tg] "+format, args...)
}

// ─── User Name Cache ─────────────────────────────────────────────────

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

// ─── Permission ───────────────────────────────────────────────────────

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

// ─── Throttled Edit Ticker ────────────────────────────────────────────

var (
	pendingText    = make(map[int64]string)
	pendingPlain   = make(map[int64]string)
	pendingEnts    = make(map[int64][]tgbotapi.MessageEntity)
	pendingMu      sync.Mutex
	editTicker     *time.Ticker
	editTickerDone chan struct{}
)

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

// ─── Send / Edit / React ──────────────────────────────────────────────

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
func doSendLong(chatID int64, text string, forceNew bool) error {
	const chunkSize = 4000

	var chunks []string
	runes := []rune(text)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		} else {
			for j := end; j > end-200 && j > i; j-- {
				if runes[j] == '\n' {
					end = j + 1
					break
				}
			}
		}
		chunks = append(chunks, string(runes[i:end]))
		i = end - chunkSize
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
			edit := tgbotapi.NewEditMessageText(chatID, prevID, plain)
			edit.Entities = entities
			if _, err := bot.Send(edit); err != nil {
				logf("edit long msg failed: %s", err.Error())
				forceNew = true
			} else {
				lastTextMu.Lock()
				lastText[chatID] = chunk
				lastTextMu.Unlock()
				continue
			}
		}

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
	return 15 * time.Second
}

func clearLastBotMsg(chatID int64) {
	lastBotMsgIDMu.Lock()
	delete(lastBotMsgID, chatID)
	lastBotMsgIDMu.Unlock()
	lastTextMu.Lock()
	delete(lastText, chatID)
	lastTextMu.Unlock()
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

func editText(chatID int64, msgID int, text string) {
	if msgID != 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
		edit.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(edit)
	} else {
		sendOrEdit(chatID, text, true)
	}
}

// ─── Typing Indicator ─────────────────────────────────────────────────

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

// ─── React Emoji ──────────────────────────────────────────────────────

func setReact(chatID int64, emoji string) {
	lastUserMsgIDMu.Lock()
	msgID, ok := lastUserMsgID[chatID]
	lastUserMsgIDMu.Unlock()
	if !ok || msgID == 0 {
		return
	}
	if bot == nil {
		return
	}
	params := tgbotapi.Params{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"message_id": strconv.Itoa(msgID),
		"reaction":   fmt.Sprintf(`[{"type":"emoji","emoji":"%s"}]`, emoji),
	}
	if _, err := bot.MakeRequest("setMessageReaction", params); err != nil {
		logf("setReact failed: %s", err.Error())
	}
}

// ─── Chat Busy Lock ───────────────────────────────────────────────────

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

// ─── Callback Store ───────────────────────────────────────────────────

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

// ─── History ──────────────────────────────────────────────────────────

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

// ─── Bot Lifecycle ────────────────────────────────────────────────────

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
	bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true})
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
