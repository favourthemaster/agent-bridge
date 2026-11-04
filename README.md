# agent-bridge

> **Talk to your coding AI agent (`agy`) from Telegram.** Run tests, watch progress in real-time, answer risky permission prompts with inline buttons, and receive files and summaries wherever you are.

---

## 1. Why This Exists

Your coding agents run in a terminal on your laptop or cloud VM. When you leave your desk, you lose them.

`agent-bridge` connects Telegram directly to a persistent, always-running `agy` agent session:
- **Out for lunch?** Text: *"Run the tests on coachly and fix failures."* Watch the live activity card update in place as it reads files, edits code, and runs builds.
- **Risky command?** Your phone buzzes with an approval card: `Allow: rm -rf build/ ?` Tap **[ Allow ]** or **[ Deny ]**.
- **Long job finished at 3 a.m.?** Your phone receives a clean summary and any generated artifacts, rather than you waking up to a dead terminal.

---

## 2. Architecture & The Single Reply-Path

```
 Telegram (Your Phone)
        │
        │ Bot API (HTTPS Long Polling - No Inbound Ports)
 ┌──────▼────────────────────────────────────────────────────────┐
 │ agent-bridge Daemon (Go)                                      │
 │                                                               │
 │  • Auth Middleware (Allowlist numeric user ID only)           │
 │  • Message Queue (FIFO when busy, silent ACK)                 │
 │  • Activity Card Manager (in-place edits <= 1 / 3s)           │
 │  • Trace Formatter (expandable blockquotes for thinking)      │
 │  • Interactive Approvals (Inline [Allow] / [Deny] buttons)    │
 │  • Formatter (Markdown -> Telegram HTML, <4096 chunking)      │
 │  • Media In/Out Watcher (inbox / outbox / /get sandbox)       │
 │  • SQLite Persistence (sessions, queue, update IDs, audit)    │
 └──────┬────────────────────────────────────────────────────────┘
        │
        │ Bidirectional PTY Stream (creack/pty)
 ┌──────▼────────────────────────────────────────────────────────┐
 │ agy Interactive Terminal Session (Always Running / Hot)       │
 │                                                               │
 │  • Persistent terminal session (zero cold-start delay)        │
 │  • Multimodal Gemini integration (reads audio, images, docs)  │
 │  • Workspace context & model memory hot in RAM                │
 └───────────────────────────────────────────────────────────────┘
```

### Why a Process Wrapper instead of an MCP Tool?
Many bridges give the agent a `send_telegram()` tool. This creates the **duplicate message problem**:
1. You text: *"Summarise the failing tests."*
2. The agent writes its answer into the session history.
3. The agent then calls `send_telegram()` with the same text.
4. You receive two near-identical messages, or the agent hallucinates, forgets to send, or burns tokens talking to itself.

`agent-bridge` wraps `agy` inside an interactive pseudo-terminal (PTY). **There is exactly one path from the agent to you, owned by the bridge.** Duplicates are impossible by design.

---

## 3. Key Capabilities

### 📊 In-Place Activity Card
While a task runs, a single message is updated in place (at most once every 3 seconds) showing live progress without flooding your phone:
```
coachly  ·  model: gemini-3.8-flash-high  ·  running 1m 12s
✔ read config/thresholds.yaml
✔ edit internal/config/load.go  (+12 -3)
✔ go build ./...  (4 s)
▶ go test ./...  (18 s so far)
```
- Retains the latest 12 steps; rolls older steps into `+N earlier steps`.
- Failed commands show `✖`, duration, exit code, and a monospace block with the last 10 lines of stderr.
- Upon completion, the card edits to `Done in Xm Ys`, and the final answer arrives as a new, notifying message.

### 💭 Collapsible Thinking Traces
Internal reasoning is enclosed in native Telegram expandable blockquotes (`<blockquote expandable>`). On mobile and desktop, it collapses into a sleek single line that you can tap to expand, keeping your screen clean and distraction-free.

### 🛡 Interactive & Auto Approvals
- **Interactive Mode**: When `agy` requests permission to run a risky command, the bridge sends an approval message with **[ Allow ]** and **[ Deny ]** buttons and a 10-minute timeout (defaulting to Deny). Tapping **Allow** injects the approval keystroke directly into the PTY.
- **Unattended Mode (`/autoapprove on`)**: When configured with `auto_approve: true` or toggled live via `/autoapprove on`, `agy` is launched with `--dangerously-skip-permissions` for 100% unattended autonomous runs. Approvals are resolved automatically with silent notices.

### 📁 Files & Multimodal Media
- **Inbound**: Photos, screenshots, PDFs, code files, and voice notes sent from Telegram are saved into `<project>/.bridge/inbox/<date>/` and passed directly to `agy`'s multimodal Gemini engine. Files sent together as an album are batched into a single instruction. Old files are automatically pruned based on `inbox_retention_days`.
- **Outbox**: Any file `agy` writes to `<project>/.bridge/outbox/` is automatically sent to your chat and moved to `.bridge/sent/`.
- **/get `<path>`**: Retrieve any file from the project workspace. Strictly sandboxed against directory traversal (`../`) and blocked against sensitive files (`.env`, `*.pem`, `id_*`, `.git/`).

### ⏳ Long-Running Task Heartbeats
For long tasks running unattended, the bridge emits a silent status heartbeat every 10 minutes (`⏳ Task still running on <project> (Xm elapsed)`), keeping you informed without ringing loud notifications. If the runner exits or crashes unexpectedly, the supervisor respawns it and dispatches an alerting notification.

---

## 4. Security & Encryption

> [!CAUTION]
> **Telegram Bot Chats Are Not End-to-End Encrypted**: Telegram Bot API traffic passes through Telegram's servers. Never send production secrets, private keys, or passwords through Telegram.

- **Strict Allowlist**: Only your numeric Telegram user ID is permitted. Everyone else is ignored silently.
- **Outbound Redaction**: All outbound messages are scanned for API keys (`sk-...`, `AIza...`, `ghp_...`), private keys, and tokens, replacing them with `[REDACTED]`.
- **Path Sandboxing**: `/get` strictly rejects path traversal and blocks sensitive files (`.env`, `*.key`, `id_rsa`, `.git/`).

---

## 5. Quickstart

### Prerequisites
- Go 1.22+
- `agy` CLI installed in `$PATH`
- A Telegram Bot Token from [@BotFather](https://t.me/BotFather)
- Your numeric Telegram User ID (obtain from [@userinfobot](https://t.me/userinfobot))

### Installation
```bash
# Clone and enter directory
git clone https://github.com/favourthemaster/agent-bridge.git
cd agent-bridge

# Build binary
go build -o bin/agent-bridge ./cmd/agent-bridge

# Copy example config
mkdir -p ~/.config/agent-bridge
cp config.example.yaml ~/.config/agent-bridge/config.yaml
```

Edit `~/.config/agent-bridge/config.yaml`:
```yaml
telegram:
  bot_token: "YOUR_BOT_TOKEN_HERE"
  allowed_user_id: 123456789

projects:
  coachly:
    path: "/home/favour/projects/coachly"
    default_model: "gemini-3.8-flash-high"
```

### Running Manually
```bash
./bin/agent-bridge --config ~/.config/agent-bridge/config.yaml
```

### Running as a Systemd Service (Always-Running)
```bash
mkdir -p ~/.config/systemd/user
cp systemd/agent-bridge.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now agent-bridge.service

# Check status
systemctl --user status agent-bridge
```

---

## 6. Telegram Command Reference

| Command | Description |
| :--- | :--- |
| `text / audio / photo` | Sent to the current `agy` session as your next instruction |
| `/project <name>` | Switch active workspace project or list configured projects |
| `/new` | Start a fresh session in the current project |
| `/status` | View current project, model, verbosity, and runner state |
| `/stop` | Interrupt the active task (the queue then carries on) |
| `/model` | Open inline keyboard to switch the active AI model |
| `/model <name>` | Switch directly to an allowed model |
| `/get <path>` | Download a file from the project directory |
| `/queue` | View waiting instructions; `/queue clear` empties it |
| `/autoapprove [on\|off]` | Toggle unattended tool execution mode live |
| `/quiet` | Show only a 1-line progress indicator and final answer |
| `/normal` | Default: live activity card + collapsible thinking traces |
| `/verbose` | Stream live tool stdout tails and raw thinking chunks |
| `/help` | Display command list |

---

## 7. Testing

Run the comprehensive test suite with race detection enabled:
```bash
go test -v -race ./...
go vet ./...
```
- **Duplicate test**: Verifies exactly one message is delivered to Telegram.
- **Flood test**: Verifies 500 rapid events are coalesced and throttled to $\le 1$ edit per 3s.
- **Approval test**: Verifies button timeouts, unauthorized click rejection, and single-click resolution.
- **Sandboxing test**: Verifies `.env`, private keys, and path escapes are blocked.
- **Redaction test**: Verifies API keys and credentials are scrubbed.

---

## License
MIT
