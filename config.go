package main

import "encoding/json"

// channelConfig holds the Telegram channel configuration received from xbot.
type channelConfig struct {
	BotToken    string `json:"bot_token"`
	AllowFrom   string `json:"allow_from"`
	AllowGroups string `json:"allow_groups"`
	AdminChatID string `json:"admin_chat_id"`
}

// cbState tracks multi-step callback state (e.g. settings input).
type cbState struct {
	Action string
	ChatID int64
}

// historyEntry is one entry in the per-chat conversation history log.
type historyEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Sender  string `json:"sender,omitempty"`
	Time    string `json:"time"`
}

// ─── WSMessage — xbot push event structure ────────────────────────────

// wsMessage represents a channel event pushed by xbot via stdin.
// These events have a "type" field but no "method" field.
type wsMessage struct {
	Type     string            `json:"type"`
	Content  string            `json:"content"`
	ChatID   string            `json:"chat_id"`
	Channel  string            `json:"channel"`
	Metadata map[string]string `json:"metadata"`
	Progress json.RawMessage   `json:"progress"`
}

// isFinal returns true if the metadata marks this event as final.
func (m *wsMessage) isFinal() bool {
	if m.Metadata == nil {
		return false
	}
	return m.Metadata["is_final"] == "true"
}

// ─── RPC structs — typed params for outbound calls ────────────────────

// sendInboundParams is the params for the "send_inbound" RPC call.
type sendInboundParams struct {
	Channel    string `json:"channel"`
	ChatID     string `json:"chat_id"`
	Content    string `json:"content"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	ChatType   string `json:"chat_type"`
}

// askUserResponseParams is the params for the "ask_user_response" RPC call.
type askUserResponseParams struct {
	Channel   string            `json:"channel"`
	ChatID    string            `json:"chat_id"`
	RequestID string            `json:"request_id"`
	Answers   map[string]string `json:"answers"`
}

// rpcRequest is a generic JSON-RPC request frame.
type rpcRequest struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// rpcResponse is a JSON-RPC response frame (received from xbot).
type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// rpcReply is a simple JSON-RPC reply frame (sent to xbot).
type rpcReply struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// rpcErrorReply is a JSON-RPC error-only reply (sent to xbot).
type rpcErrorReply struct {
	Error string `json:"error"`
}

// ─── Progress sub-types ───────────────────────────────────────────────

// configSchemaEntry represents one field in the channel config schema.
type configSchemaEntry struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Description  string `json:"description,omitempty"`
	Type         string `json:"type"`
	DefaultValue string `json:"default_value,omitempty"`
	Category     string `json:"category,omitempty"`
}

// progressEvent represents the progress payload inside a WSMessage.
type progressEvent struct {
	Phase          string             `json:"phase"`
	Thinking       string             `json:"thinking"`
	Reasoning      string             `json:"reasoning"`
	StreamContent  string             `json:"stream_content"`
	ActiveTools    []progressTool     `json:"active_tools"`
	CompletedTools []progressTool     `json:"completed_tools"`
	Questions      []progressQuestion `json:"questions"`
	RequestID      string             `json:"request_id"`
}

// progressTool represents a single tool entry in progress events.
type progressTool struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// progressQuestion represents an AskUser question in progress events.
type progressQuestion struct {
	Question string `json:"question"`
	Options  []any  `json:"options"`
}
