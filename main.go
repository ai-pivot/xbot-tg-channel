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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ai-pivot/xbot/plugin/protocol"
)

// ─── RPC State ────────────────────────────────────────────────────────

var (
	rpcID   int64
	rpcIDMu sync.Mutex

	rpcPending   = make(map[string]chan json.RawMessage)
	rpcPendingMu sync.Mutex
)

// ─── JSON-RPC Helpers ─────────────────────────────────────────────────

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

func callRPC(method string, params any) (json.RawMessage, error) {
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

	writeJSON(&rpcRequest{
		ID:     id,
		Method: method,
		Params: params,
	})

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("RPC timeout: %s", method)
	}
}

func sendInbound(chatID int64, content string, senderID int64, senderName string, ctype string) {
	clearLastBotMsg(chatID)
	writeJSON(&rpcRequest{
		ID:     nextRPCID(),
		Method: "send_inbound",
		Params: sendInboundParams{
			Channel:    "tg",
			ChatID:     fmt.Sprintf("tg:%d", chatID),
			Content:    content,
			SenderID:   strconv.FormatInt(senderID, 10),
			SenderName: senderName,
			ChatType:   ctype,
		},
	})
}

func sendAskUserResponse(chatID int64, requestID string, answers map[string]string) {
	writeJSON(&rpcRequest{
		ID:     nextRPCID(),
		Method: "ask_user_response",
		Params: askUserResponseParams{
			Channel:   "tg",
			ChatID:    fmt.Sprintf("tg:%d", chatID),
			RequestID: requestID,
			Answers:   answers,
		},
	})
}

func sendRPCResponse(reqID string, result any) {
	writeJSON(&rpcReply{ID: reqID, Result: result})
}

func sendRPCError(reqID string, errMsg string) {
	writeJSON(&rpcReply{ID: reqID, Error: errMsg})
}

// ─── Activate Handler (using SDK types) ───────────────────────────────

func handleActivate() *protocol.ActivateResult {
	configSchema := []configSchemaEntry{
		{Key: "enabled", Label: "Enable", Type: "toggle", DefaultValue: "false", Category: "Telegram"},
		{Key: "bot_token", Label: "Bot Token", Description: "From @BotFather", Type: "password", Category: "Telegram"},
		{Key: "allow_from", Label: "Allowed Users", Description: "Comma-separated user IDs (empty=all)", Type: "text", Category: "Telegram"},
		{Key: "allow_groups", Label: "Allowed Groups", Description: "Comma-separated group IDs (empty=all)", Type: "text", Category: "Telegram"},
		{Key: "admin_chat_id", Label: "Admin Chat ID", Description: "Optional", Type: "text", Category: "Telegram"},
	}
	schemaJSON, _ := json.Marshal(configSchema)

	return &protocol.ActivateResult{
		Result: "ok",
		ChannelProvider: &protocol.ChannelProviderDecl{
			Name:         "tg",
			ConfigSchema: schemaJSON,
		},
	}
}

// ─── Incoming Message Dispatch ────────────────────────────────────────

// handleIncoming parses one JSON line from stdin and dispatches it:
//  1. RPC response (has "id" but no "method"/"type") → deliver to pending callRPC
//  2. Plugin request (has "method" but no "id") → old-style activate etc.
//  3. RPC request from xbot (has "id" + "method") → dispatch via SDK-like logic
//  4. Channel event push (has "type") → handleXbotEvent
func handleIncoming(line string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}

	// Detect fields
	_, hasID := raw["id"]
	_, hasMethod := raw["method"]
	_, hasType := raw["type"]

	// 1. RPC response from xbot → deliver to pending callRPC
	if hasID && !hasMethod && !hasType {
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return
		}
		rpcPendingMu.Lock()
		ch, ok := rpcPending[resp.ID]
		rpcPendingMu.Unlock()
		if ok {
			data, _ := json.Marshal(resp)
			select {
			case ch <- data:
			default:
			}
		}
		return
	}

	// 2. Old-style plugin request (method, no id) — e.g. "activate"
	if hasMethod && !hasID {
		var method string
		json.Unmarshal(raw["method"], &method)
		if method == "activate" {
			writeJSON(handleActivate())
		} else {
			writeJSON(&rpcErrorReply{Error: fmt.Sprintf("unknown method: %s", method)})
		}
		return
	}

	// 3. RPC request from xbot (id + method)
	if hasID && hasMethod {
		var idStr, method string
		json.Unmarshal(raw["id"], &idStr)
		json.Unmarshal(raw["method"], &method)

		// Dispatch using SDK logic
		switch method {
		case "activate":
			writeJSON(handleActivate())
		case "deactivate":
			shutdownOnce.Do(func() { close(stopCh) })
			stopEditTicker()
			writeJSON(&protocol.Response{})
		default:
			sendRPCResponse(idStr, "ok")
		}
		return
	}

	// 4. Event push from xbot (type field) — WSMessage
	if hasType {
		var msg wsMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return
		}
		handleXbotEvent(&msg)
	}
}

// ─── Main ─────────────────────────────────────────────────────────────

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
