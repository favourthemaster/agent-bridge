# agent-bridge Security Guide

Because `agent-bridge` allows a chat message to execute shell commands and modify code on your machine, security is a first-class requirement.

---

## 1. Authentication & Sender Allowlist

- **Numeric User ID Only**: The bridge checks `update.Message.From.ID == allowed_user_id` and `update.CallbackQuery.From.ID == allowed_user_id`.
- Usernames can be changed in Telegram; numeric user IDs are permanent and immutable.
- All messages from any other Telegram user, bot, or group chat are **silently dropped** without any acknowledgement, logs of message content, or error responses.

---

## 2. Encryption & Privacy Notice

> [!CAUTION]
> **Telegram Bot Chats Are Not End-to-End Encrypted**: Messages between a user and a Telegram bot pass through Telegram's servers. Do not transmit production secrets, private keys, or passwords through Telegram.

---

## 3. Outbound Secret Redaction

Before any message is sent to Telegram, `internal/redact` runs a regex pass that detects and scrubs:
- OpenAI API keys (`sk-...`)
- Anthropic API keys (`sk-ant-...`)
- Google Gemini / Cloud API keys (`AIza...`)
- GitHub Personal Access Tokens (`ghp_...`, `github_pat_...`)
- Telegram bot tokens
- RSA, EC, and OpenSSH private key blocks (`-----BEGIN ... PRIVATE KEY-----`)
- Generic secret assignments (`api_key: ...`, `password: ...`)

---

## 4. Interactive Permissions & Approvals

- Any risky tool invocation requiring confirmation emits an approval prompt with inline buttons: `[ Allow ]` and `[ Deny ]`.
- Buttons are tagged with an ephemeral UUID and expire after **10 minutes**.
- If no button is pressed within 10 minutes, the request defaults to **Deny** and is marked as expired.
- Tapping **Allow** writes approval keystrokes directly into the PTY and logs the decision to the local SQLite audit log.

---

## 5. File Sandboxing & Sensitive Deny-List

- **Inbox Confinement**: Files sent from Telegram are saved into `<project>/.bridge/inbox/<date>/`. They are never marked executable and are treated as passive input data.
- **`/get <path>` Confinement**:
  - Validates that the requested path does not escape the project folder via directory traversal (`../`).
  - Blocks `.git/` folder access.
  - Rejects files matching the sensitive deny-list:
    - `.env`, `.env.*`
    - `*.pem`, `*.key`, `*.p12`, `*.pfx`
    - `id_rsa*`, `id_ed25519*`, `id_ecdsa*`
    - `*.secret*`
    - `credentials.json`
