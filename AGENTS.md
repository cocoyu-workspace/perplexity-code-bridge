# Perplexity Code Bridge Development Guidelines

- Work on named `codex/` branches and deliver through pull requests linked to a GitHub Issue (`Fixes #<id>` or `Refs #<id>`).
- Never commit `config.yaml`, `accounts/*.json` (except `accounts/account.example.json`), cookies, session tokens, API keys, runtime logs, or compiled binaries.
- Never expose upstream provider branding (`Perplexity`, `perplexity.ai`, `pplx`) in client-facing error classifications or model reminder strings. Keep `TestClientFacingErrorsDoNotExposeBranding` passing.
- Preserve 100% local Claude Code tool execution (`LOCAL ACTION PROTOCOL` -> model JSON -> schema validation -> `tool_use` SSE -> client `tool_result` -> multi-turn thread continuation via `perplexityThreadRegistry`).
- Before submitting changes, run:
  - `go test ./... -count=1`
  - `go build -o perplexity-bridge ./cmd/perplexity-bridge && rm -f perplexity-bridge`
  - `python3 -m unittest discover -s tests -v`
