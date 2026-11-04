# Architecture Decision Records (ADR)

## ADR 1: Wrapper / PTY vs. MCP Tool (The Duplicate Problem)

### Context
There are two architectural approaches to connecting an AI agent to a chat app:
1. **Option A (The Agent Sends Messages Itself via an MCP Tool)**: The agent has a `send_telegram(text)` tool and decides when to call it.
2. **Option B (The Bridge Owns the Session and Relays Output)**: The bridge wraps the agent CLI as a child process, feeds input in, and relays output out.

### The Duplicate Problem
If Option A is chosen:
1. You text: "Run the tests on coachly and summarize failures."
2. The agent executes the tests and generates its normal response summary in the conversation.
3. The agent then calls `send_telegram(summary)`.
4. The user receives two duplicate messages, or the agent hallucinates, forgets to send, or burns extra token context talking to itself.

### Decision
**We choose Option B.** The bridge owns the conversation channel. There is exactly one path from the agent to the user, owned by the bridge. Duplicates cannot happen by design.

---

## ADR 2: Tech Stack Choice (Go vs. Node/Python)

### Context
The bridge daemon runs 24/7 on a VM or laptop alongside coding agents. Memory usage, CPU efficiency, and process startup time are critical.

### Decision
We built `agent-bridge` in **Go**:
- **Single Static Binary**: No Node.js runtime, no `node_modules` directory, no Python virtualenv dependencies.
- **Resource Footprint**: Consumes ~15 MB of RAM compared to ~150-200 MB for a Node.js daemon.
- **Native Concurrency**: Go goroutines and channels provide the ideal concurrency model for handling Telegram long polling, debounced activity cards, timers, and PTY stream reading.
- **Pure Go SQLite**: Uses `modernc.org/sqlite` (CGO-free SQLite), making cross-compilation and deployments effortless.

---

## ADR 3: Always-Running Persistent PTY vs. Cold Process Spawning

### Context
Spawning `agy` afresh for every turn takes ~8–13 seconds of cold startup to initialize tools, load configurations, and restore context.

### Decision
`agent-bridge` maintains `agy` running continuously in a persistent pseudo-terminal (`creack/pty`).
- The session stays hot in memory with zero cold-start delay when a message arrives from your phone.
- If `agy` ever crashes or is terminated, the supervisor automatically restarts it.
- When you are at your desk, you can observe or attach to the terminal; when you walk away, Telegram takes over seamlessly.

---

## ADR 4: Inbound Multimodal Gemini vs. Local CPU Transcription

### Context
`agy` is powered by Gemini, which natively accepts multimodal input (audio, images, PDFs, code). Running local speech-to-text models like Whisper on a dual-core laptop CPU takes 12–18 seconds for a 5-second audio clip and consumes 100% CPU.

### Decision
Incoming voice notes and audio clips are saved to `.bridge/inbox/` as OGG/Opus files and passed directly to `agy`'s multimodal context. This yields instant response times, zero CPU strain, and higher transcription accuracy.

---

## ADR 5: Telegram HTML Parse Mode vs. MarkdownV2

### Context
Telegram's `MarkdownV2` parse mode requires escaping almost every ASCII punctuation character (`_`, `*`, `[`, `]`, `(`, `)`, `~`, `` ` ``, `>`, `#`, `+`, `-`, `=`, `|`, `{`, `}`, `.`, `!`). When coding agents emit code snippets or diffs, MarkdownV2 frequently fails with parsing errors.

### Decision
`agent-bridge` converts all agent Markdown to **Telegram HTML**:
- Only `<`, `>`, and `&` require escaping in plain text.
- Standard tags (`<b>`, `<i>`, `<s>`, `<code>`, `<pre>`, `<blockquote>`, `<blockquote expandable>`) are robust and cleanly supported across all official Telegram clients.
- A fallback automatically retries sending as sanitized plain text if Telegram ever rejects a payload.
