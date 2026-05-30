package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ─── Telegram Update Handling ─────────────────────────────────────────

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

	logf("TG msg: chatID=%d chatType=%s chatTitle=%q userID=%d text=%q",
		chatID, chatTypeStr(chat), chat.Title,
		func() int64 {
			if user != nil {
				return user.ID
			}
			return 0
		}(),
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

	lastUserMsgIDMu.Lock()
	lastUserMsgID[chatID] = msg.MessageID
	lastUserMsgIDMu.Unlock()

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

func isMentionedOrReply(msg *tgbotapi.Message) bool {
	if bot == nil {
		return false
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.ID == bot.Self.ID {
		return true
	}
	if strings.Contains(msg.Text, "@"+bot.Self.UserName) {
		return true
	}
	return false
}

// ─── Commands ─────────────────────────────────────────────────────────

func onCommand(chatID int64, user *tgbotapi.User, cmd string) {
	if user != nil && !isAllowed(user.ID) {
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

// ─── Callback Query Handling ──────────────────────────────────────────

func onCallbackQuery(cb *tgbotapi.CallbackQuery) {
	if cb == nil || cb.Data == "" {
		return
	}
	data := cb.Data
	chatID := cb.Message.Chat.ID

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
	parts := strings.SplitN(data, ":", 4)
	if len(parts) < 4 {
		return
	}
	requestID := parts[1]
	answer := parts[3]

	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, tgbotapi.InlineKeyboardMarkup{})
	bot.Send(edit)

	sendAskUserResponse(chatID, requestID, map[string]string{"0": answer})
	logf("ask_user answered: request=%s answer=%s", requestID, truncate(answer, 50))
}

// ─── Settings Panel ───────────────────────────────────────────────────

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
	resp, err := callRPC("list_subscriptions", struct{}{})
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

// ─── AskUser Panel ────────────────────────────────────────────────────

func handleAskUser(chatID int64, questions []progressQuestion, requestID string) {
	if len(questions) == 0 {
		return
	}

	q := questions[0]

	var sb strings.Builder
	sb.WriteString("❓ *")
	sb.WriteString(q.Question)
	sb.WriteString("*")

	if len(q.Options) > 0 {
		var buttons [][]tgbotapi.InlineKeyboardButton
		for _, o := range q.Options {
			optStr := fmt.Sprintf("%v", o)
			data := fmt.Sprintf("ask:%s:0:%s", requestID, optStr)
			buttons = append(buttons, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(optStr, data),
			})
		}
		if _, err := sendMessageWithKeyboard(chatID, sb.String(), buttons); err != nil {
			logf("ask_user send failed: %v", err)
		}
	} else {
		sendOrEdit(chatID, sb.String()+"\n\n_Please reply with your answer._", true)
		storeCallback(chatID, fmt.Sprintf("ask:%s:0", requestID))
	}
}

// ─── xbot Event Handling ──────────────────────────────────────────────

func handleXbotEvent(msg *wsMessage) {
	chatID := parseChatID(msg.ChatID)

	logf("EVENT type=%q chat_id=%q content_len=%d has_progress=%v",
		msg.Type, msg.ChatID, len(msg.Content), msg.Progress != nil)

	switch msg.Type {
	case "channel_config":
		if msg.Metadata != nil {
			if cfgStr, ok := msg.Metadata["config"]; ok {
				json.Unmarshal([]byte(cfgStr), &cfg)
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
		if msg.Type == "text" && msg.Content != "" {
			display = msg.Content
		} else if msg.Progress != nil {
			var pe progressEvent
			if json.Unmarshal(msg.Progress, &pe) == nil {
				if pe.Phase == "done" {
					isDone = true
				}
				display = formatProgress(&pe)

				// Check AskUser
				if len(pe.Questions) > 0 {
					handleAskUser(chatID, pe.Questions, pe.RequestID)
				}
			}
		}

		if display == "" {
			display = "💭 思考中..."
		}

		isFinal := msg.Type == "text" && msg.isFinal()

		// phase:done with thinking = final reply from LLM
		if isDone && display != "" && display != "💭 思考中..." {
			_ = sendOrEdit(chatID, display, false)
			addHistory(chatID, "assistant", display, "")
			cancelTyping(chatID)
		} else if isFinal && msg.Content != "" {
			_ = sendOrEdit(chatID, msg.Content, false)
		} else {
			_ = sendOrEdit(chatID, display, false)
		}

		// Final text event → flush pending, add to history, release busy lock
		if isFinal {
			flushPatch(chatID)
			cancelTyping(chatID)
			if msg.Content != "" {
				addHistory(chatID, "assistant", msg.Content, "")
			}
			setReact(chatID, "✅")
			releaseBusy(chatID)
		}
		// phase:done also flush and release
		if isDone && display != "" && display != "💭 思考中..." {
			flushPatch(chatID)
			setReact(chatID, "✅")
			releaseBusy(chatID)
		}
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────

func parseChatID(s string) int64 {
	s = strings.TrimPrefix(s, "tg:")
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}
