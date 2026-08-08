# Telegram channel review

Date: 2026-08-08

Scope: review and correction of the Telegram channel implementation on
`feat/telegram-integration`, against `upstream/main` / `0db0055964e912d04b4a1f6e3ae6a83d0336731d` and the repository contribution rules.

The initial review baseline was commit
`e512ad3deb60b24d6089f1938bb4ded683634f03`. The source archive sent for the
external review was 281,285 bytes with SHA-256
`9f845d37a911788131e543d5dec3a61a09965d3209f84a8cef47f3574fbf64f1`.

## Findings and corrections

- Refused long-polling installation when Telegram has an outgoing webhook, so
  installation cannot appear successful while polling returns 409. The
  adapter does not silently delete another integration's webhook. See the
  [Telegram Bot API](https://core.telegram.org/bots/api) for the mutual
  exclusivity of `getUpdates` and outgoing webhooks.
- Removed bot-token exposure from transport error strings.
- Made Telegram outbound delivery fail closed when task lookup or channel
  provenance lookup fails, and aligned the discriminator with the shared
  channel-ingestion provenance query.
- Prevented group-visible bearer binding links; group users are directed to
  start a private chat first. Redemption also validates the shared token's
  channel type.
- Counted outbound message chunks in UTF-16 units and restricted plain-text
  fallback to HTML entity parsing errors, avoiding oversized messages and
  duplicate sends after ambiguous transport failures.
- Made malformed HTTP 200 responses fail in the Telegram settings and binding
  flows instead of showing false success; query failures now render an error
  state. Added all locale keys and brought Telegram UI classes onto the
  repository's semantic type scale.

## Verification

Passed:

- `make test` (including Go race tests and the agent CLI guard); the Telegram
  package passed in the full run.
- `pnpm typecheck`.
- `pnpm lint` (existing warnings only).
- Telegram package tests, Telegram settings tests (10 tests), locale parity,
  and Web type-scale tests.
- Isolated worktree: Telegram Go tests, views typecheck, Telegram settings
  tests, and Web type-scale tests.

Not fully verifiable in this environment:

- `pnpm test` reached 22 Web files / 195 tests and 43 Desktop files / 438
  tests, but the Desktop suite had one environment failure because Electron
  was not installed correctly.
- `pnpm build` was blocked by the existing missing desktop
  `@fontsource-variable/geist-mono` dependency and unavailable
  `fonts.googleapis.com` during Web/Docs builds.
- No real Telegram bot token or production Telegram API smoke test was used;
  tests use local HTTP fixtures only. No E2E Telegram flow was run.

The external ChatGPT Pro review conversation was
https://chatgpt.com/c/6a773603-2798-83ec-a368-e298da9da653. It identified the
webhook, provenance fail-open, group binding, and frontend false-success risks,
but its final response stalled; the patch and acceptance decision were made
from the repository and test evidence in this checkout.
