# agent-bridge Architecture

`agent-bridge` is a high-performance daemon written in Go that connects your Telegram chat directly to a persistent, always-running `agy` coding agent session.

```
Telegram (Your Phone)
       │
       │ Bot API (Long Polling, TLS)
┌──────▼────────────────────────────────────────────────────────┐
│ agent-bridge Daemon (Go)                                      │
│                                                               │
│  • Auth Middleware (Allowlist numeric user ID only)           │
│  • Message Queue (FIFO when busy, silent ACK)                 │
│  • Activity Card Manager (in-place edits <= 1 / 3s)           │
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

## Core Modules

### 1. `internal/telegram`
- Uses `github.com/go-telegram/bot` over HTTPS long polling (no inbound open port or public webhook needed).
- `AuthMiddleware` verifies that every message and button click originates from `telegram.allowed_user_id`. Unauthenticated users are ignored silently.
- `ActionTicker` refreshes the `typing` chat action every 4s while `agy` is processing, stopping automatically when idle or waiting for an approval.
- `CommandHandler` handles `/project`, `/new`, `/status`, `/stop`, `/model`, `/get`, `/queue`, `/quiet`, `/normal`, `/verbose`, and `/help`.

### 2. `internal/runner`
- `PTYRunner`: Spawns `agy` in an interactive pseudo-terminal (`creack/pty`). It keeps the process running continuously. If `agy` exits unexpectedly, the supervisor restarts it automatically.
- Bidirectional stream: Writes instructions + `\r` directly to the terminal master fd; injects approval keystrokes (`1\r` / `2\r`).
- `FakeRunner`: An in-memory scriptable runner for deterministic unit and end-to-end testing.

### 3. `internal/parser`
- `vt_strip.go`: Strips ANSI/VT100 escape codes, cursor positioning, and alt-screen buffer switches.
- `stream_parser.go`: Detects when `agy` is ready for the next prompt (`EventPromptReady`), extracts approval requests (`Allow: <command>?`), and tracks tool execution steps.

### 4. `internal/activity`
- In-place edited progress card that summarizes active and completed steps.
- Throttled to at most once every 3 seconds to avoid Telegram 429 rate limits.
- Folds older steps when more than 12 steps have run into `+N earlier steps`.
- Displays failures with `✖`, duration, exit code, and monospace block for last 10 lines of stderr.

### 5. `internal/queue`
- If you text while `agy` is busy, the message is queued in SQLite and acknowledged silently with `⏳ Queued (N ahead)`.
- Never injects mid-task.
- As soon as the active task finishes, the queue automatically dispatches the next item.

### 6. `internal/media`
- Inbound: Voice notes, screenshots, images, and documents are saved under `<project>/.bridge/inbox/<date>/`. Files arriving within 2 seconds are batched into a single instruction.
- Outbound: Watches `<project>/.bridge/outbox/`. When a file has stopped modifying for 2 seconds, sends it to Telegram as a photo, audio, video, or document, then moves it to `.bridge/sent/`.
- `/get <path>`: Confinement check ensures path cannot escape the project directory; blocks sensitive files (`.env`, `*.pem`, `id_*`, `.git/`).

### 7. `internal/redact`
- Outbound scrubber that redacts OpenAI, Anthropic, Gemini, GitHub, Telegram, and private keys before any message is sent to Telegram.
