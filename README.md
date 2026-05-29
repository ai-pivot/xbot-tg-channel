> **本文档面向 xbot agent**。当用户请求安装 Telegram channel 插件时，按以下步骤操作。

# xbot-tg-channel

Telegram channel plugin for xbot。通过 Telegram Bot 与用户通信，支持消息原地更新（patch）、群组、Markdown 渲染、命令菜单。

## 安装

### 1. 确定安装目录

插件目录为 xbot plugin 目录下的 `xbot.tg-channel/` 子目录。不要写死绝对路径，使用 xbot 的 plugin 目录配置或通过 `plugin-creator` skill 获取正确路径。

典型路径结构：
```
<xbot-plugin-dir>/
  xbot.tg-channel/
    plugin.json
    xbot-tg-channel    # 可执行文件，与 plugin.json 同目录
```

### 2. 获取二进制

从 [GitHub Releases](https://github.com/ai-pivot/xbot-tg-channel/releases) 下载对应平台的预编译二进制。无需 Go 环境。

选择与当前系统匹配的文件（运行 `uname -sm` 确认）：
- `xbot-tg-channel-linux-amd64`
- `xbot-tg-channel-linux-arm64`
- `xbot-tg-channel-darwin-amd64`
- `xbot-tg-channel-darwin-arm64`

下载后重命名为 `xbot-tg-channel`（去掉平台后缀），放入插件目录，设置可执行权限 `chmod +x`。

### 3. 激活 skill

安装完成后，使用 `skill-creator` skill 激活此插件创建的 skill（如果存在 skill 定义文件）。skill 文件位于插件目录或 xbot 的 skills 目录中。

### 4. 配置 Bot Token

需要用户提供 Telegram Bot Token。引导用户：

1. 在 Telegram 中搜索 [@BotFather](https://t.me/BotFather)
2. 发送 `/newbot`，按提示创建 bot
3. 复制获得的 `bot_token`
4. 将 token 写入 xbot config 的 channels.tg.bot_token 字段

配置结构：
```json
{
  "channels": {
    "tg": {
      "bot_token": "<用户提供>",
      "allow_from": "",
      "allow_groups": ""
    }
  }
}
```

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `bot_token` | BotFather 提供的 token（必填） | — |
| `allow_from` | 允许的用户 ID，逗号分隔，空=所有人 | `""` |
| `allow_groups` | 允许的群组 ID（负数），逗号分隔，空=所有群 | `""` |

### 5. 重启 xbot 使插件生效

## 验证

安装完成后在 Telegram 中向 bot 发送一条消息，应收到回复。

## 特性

- 消息原地更新（progress patch，类似飞书卡片）
- Markdown 渲染（粗体、斜体、代码块、表格、链接等）
- 群组支持（@提及或回复触发）
- 限流控制（每 chat 每秒最多 1 次 edit，429 自动重试）
- 长消息自动拆分（4096 字符限制）
- 命令：`/start` `/cancel` `/new` `/settings` `/history`
- Inline keyboard 快捷操作
- 每 chat 同时只处理一个请求，排队机制

## 从源码编译

需要 Go 1.21+：`go build -o xbot-tg-channel .`

## License

MIT
