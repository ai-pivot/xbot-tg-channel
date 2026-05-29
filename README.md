# xbot-tg-channel

Telegram channel plugin for [xbot](https://github.com/fixlater/xbot). Enables xbot to communicate via Telegram bot, with message patching (progress updates in-place), group support, markdown rendering, and inline keyboard commands.

## Installation

No Go toolchain required. Download the pre-built binary from [GitHub Releases](https://github.com/ai-pivot/xbot-tg-channel/releases) for your platform.

### Step-by-step (run these commands)

```bash
# 1. Create plugin directory
mkdir -p ~/.xbot/plugins/xbot.tg-channel

# 2. Download the latest binary (replace OS/ARCH with your platform)
# Linux amd64:
curl -L -o ~/.xbot/plugins/xbot.tg-channel/xbot-tg-channel \
  https://github.com/ai-pivot/xbot-tg-channel/releases/latest/download/xbot-tg-channel-linux-amd64
# Linux arm64:
curl -L -o ~/.xbot/plugins/xbot.tg-channel/xbot-tg-channel \
  https://github.com/ai-pivot/xbot-tg-channel/releases/latest/download/xbot-tg-channel-linux-arm64
# macOS amd64:
curl -L -o ~/.xbot/plugins/xbot.tg-channel/xbot-tg-channel \
  https://github.com/ai-pivot/xbot-tg-channel/releases/latest/download/xbot-tg-channel-darwin-amd64
# macOS arm64:
curl -L -o ~/.xbot/plugins/xbot.tg-channel/xbot-tg-channel \
  https://github.com/ai-pivot/xbot-tg-channel/releases/latest/download/xbot-tg-channel-darwin-arm64

# 3. Make it executable
chmod +x ~/.xbot/plugins/xbot.tg-channel/xbot-tg-channel

# 4. Configure your bot token
# Edit ~/.xbot/config.json and add under "channels":
#   "tg": {
#     "bot_token": "YOUR_BOT_TOKEN_FROM_BOTFATHER",
#     "allow_from": ""
#   }
```

### Getting a Bot Token

1. Message [@BotFather](https://t.me/BotFather) on Telegram
2. Send `/newbot` and follow the prompts
3. Copy the `bot_token` you receive
4. Set it in your xbot config

## Configuration

| Key | Description | Default |
|-----|-------------|---------|
| `bot_token` | Bot token from @BotFather | (required) |
| `allow_from` | Comma-separated Telegram user IDs (empty = allow all) | `""` |
| `allow_groups` | Comma-separated Telegram group IDs (negative, e.g. `-1001234567`) | `""` (allow all) |

To allow specific users only, set `allow_from` to their numeric user IDs (visible in the bot's debug log or via Telegram API).

To restrict group access, set `allow_groups`. Leave empty to allow all groups.

## Features

- **Message patching**: Progress updates edit the same message in-place (like Feishu cards)
- **Markdown rendering**: Full markdown support via Telegram entities (bold, italic, code, tables, links, etc.)
- **Group support**: Works in groups with @mention or reply triggering
- **Rate limiting**: Throttled edits (1/sec per chat) with automatic retry on rate limit
- **Long messages**: Auto-splits messages exceeding 4096 characters
- **Commands**: `/start`, `/cancel`, `/new`, `/settings`, `/history`
- **Inline keyboard**: Quick actions via button taps
- **Session queue**: One conversation per chat at a time, new requests wait politely

## Commands

| Command | Description |
|---------|-------------|
| `/start` `/help` | Show welcome message with inline keyboard |
| `/cancel` | Cancel current request |
| `/new` | Start a new conversation |
| `/settings` | Manage LLM configuration (model, temperature, etc.) |
| `/history` | View conversation history |

## Architecture

The plugin runs as a standalone binary communicating with xbot via **JSON-RPC over stdio** (the GrpcPluginTransport protocol). It uses long-polling to receive Telegram updates and converts them to xbot inbound messages. Outbound messages from xbot are sent as Telegram messages with proper markdown formatting.

## Build from Source

Requires Go 1.21+.

```bash
go build -o xbot-tg-channel .
```

## License

MIT
