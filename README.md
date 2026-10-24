# Perplexity Code Bridge

Standalone Anthropic `/v1/messages` local proxy and Claude Code CLI launcher (`claude-pplx.py`) backed by Perplexity Web upstream (`POST /rest/sse/perplexity_ask`).

## Features

- **100% Local Claude Code Tool Execution**: Translates Anthropic `tools[]` (`Read`, `Write`, `Edit`, `Bash`, `Glob`, `Grep`, `Agent`, `Skill`, `ToolSearch`, `SendMessage`, plus `<available-deferred-tools>`) into a compact `LOCAL ACTION PROTOCOL`, validates model JSON tool calls against schemas, emits standard `tool_use` SSE blocks, and resumes threads with `tool_result`.
- **Multi-Turn Cloud Thread Continuation**: Automatically binds and resumes upstream threads via `last_backend_uuid` + `read_write_token` + `frontend_context_uuid` (`perplexityThreadRegistry`), sending only incremental turns after Turn 1 and supporting up to 1M tokens of native cloud thread context.
- **Refusal Detection & Replay Recovery**: Detects false "cloud-only / no local tool access" refusals and automatically replays the full conversation on a fresh upstream thread with a strict correction reminder.
- **Multi-Account Pool & Tier Routing**: Supports multiple Pro / Max accounts in `accounts/`, automatically routing `sonnet-5.5` / `gpt-6.1-sol` to Pro accounts first (`remaining_pro` weighted) and reserving `opus-5.5` for Max accounts.
- **Zero Upstream Branding in Client Surfaces**: Scrubs upstream provider branding from client-facing error classifications and system reminders.

## Quick Start

1. **Configure server & account**:
   ```bash
   cp config.example.yaml config.yaml
   cp accounts/account.example.json accounts/my_account.json
   ```
   Fill in `session_token` (value of `__Secure-next-auth.session-token` from an authenticated browser session) or `raw_cookie` in `accounts/my_account.json`.

2. **Build & run the bridge** (or let `claude-pplx.py` auto-start it):
   ```bash
   go build -o perplexity-bridge ./cmd/perplexity-bridge
   ./perplexity-bridge
   ```

3. **Launch Claude Code CLI**:
   ```bash
   python3 claude-pplx.py
   # Or select a specific model / effort:
   python3 claude-pplx.py --model opus-5.5 --effort high
   python3 claude-pplx.py --model gpt-6.1-sol
   ```

## Supported Models

| CLI Model ID | Upstream Target | Tier Requirement | Context Window |
| :--- | :--- | :--- | :--- |
| `sonnet-5.5` / `claude-sonnet-5` | `claude55sonnet` / `claude55sonnetthinking` | Pro / Max | 200K (`1M` cloud thread) |
| `opus-5.5` / `claude-opus-5-5` | `claude55opus` / `claude55opusthinking` | Max | 200K (`1M` cloud thread) |
| `gpt-6.1-sol` / `claude-sol-6-1` | `gpt6_1_sol` / `gpt6_1_sol_thinking` | Pro / Max | 1M |

## HTTP Endpoints

- `POST /v1/messages` — Anthropic Messages API (streaming & non-streaming)
- `POST /v1/messages/count_tokens` — Local token estimation
- `GET /v1/models` — Model catalog
- `GET /health` — Health check
- `GET /admin/accounts` — Account pool status & quota snapshot
- `POST /admin/accounts/reload` — Reload accounts from disk
- `POST /admin/accounts/refresh` — Trigger upstream quota & session refresh

## Testing

```bash
go test ./... -count=1
python3 -m unittest discover -s tests -v
```

## Documentation

- [`docs/perplexity-local-tools.md`](docs/perplexity-local-tools.md) — Local tool bridge contract, chunking, and refusal recovery
- [`docs/perplexity-billing-and-limits.md`](docs/perplexity-billing-and-limits.md) — Upstream subscription tiers, quota mechanics, and context window benchmarks
