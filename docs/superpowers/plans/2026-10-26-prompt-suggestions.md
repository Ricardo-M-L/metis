# Next-Turn Prompt Suggestions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Offer an optional gray next-turn prompt after successful completion in both the terminal and Desktop; Tab accepts it into the draft without sending it.

**Architecture:** A small shared `internal/promptsuggest` package extracts a bounded textual exchange and makes a cancellable, tool-free request through the existing provider abstraction. Terminal state stays on the Bubble Tea goroutine; Desktop obtains suggestions through a session-bound HTTP endpoint and keeps ghost text separate from the textarea value. Existing slash/file completion, input drafts, session ownership and model routing remain authoritative.

**Tech Stack:** Existing Go provider interfaces, Bubble Tea v2, the shared Desktop/WebUI Go server, vanilla JavaScript and existing CSS tokens. No new dependencies.

**Spec:** User request on 2026-10-26: gray next-round suggestion after output, Tab makes it normal editable input; both terminal and Desktop; implement directly. Implementation choices below are defaults selected by the implementer, not additional user requirements.

## Global Constraints

- Do not commit, push, tag or publish this new feature without a new explicit request.
- Do not modify the original dirty checkout; work in this existing isolated checkout.
- Put new logic in focused files/packages rather than enlarging existing main files beyond small wiring changes.
- Suggestions are ephemeral: never add them to conversation history, execute tools, or send them until the user explicitly accepts and submits.
- Generate only after successful complete user turns, with no draft, attachments, queue or pending interaction. Failures and cancellations do not produce suggestions.
- Use the current session's existing provider/model; do not introduce a fixed auxiliary model or credential route.
- Bound requests to 8 seconds, 256 output tokens and 4096 runes per side of the exchange; output is one plain-text suggestion of at most 160 runes or no suggestion.
- Add `ui.prompt_suggestions`, enabled by default, with a visible settings control and documentation of the additional short model request.
- Never overwrite a draft or accept an expired suggestion. Tab accepts; Enter still sends only real input. Preserve IME behavior and ordinary Tab navigation when no suggestion is available.
- Use deterministic fake providers for automated tests and live UI smoke tests; no paid model requests are needed.

## Review Focus

- A delayed result arrives after typing/deleting, switching sessions/models, or beginning another turn: discard it.
- A provider iteration finishes while the agent is still using tools: do not mistake `turn_end` for full task completion.
- A modal, slash/file completion, IME composition, or attachment is active: do not steal keys or replace the draft.
- Isolated Desktop workers use a different session/model from the server's current loop: bind provider and history to the requested completed session.
- A provider fails, emits tool calls/control sequences, or has no sensible follow-up: silently omit the suggestion without corrupting history or UI state.

---

### Task 1: Shared generator and configuration

**Files:** Create `internal/promptsuggest/suggestion.go` and `suggestion_test.go`; modify `internal/config/config.go` and the existing user-settings definitions/tests.

**Interfaces:** `type Exchange struct { User, Assistant string }`; `LatestExchange([]provider.Message) (Exchange, bool)`; `Generate(context.Context, provider.Provider, string, Exchange) (string, error)`. The generator only consumes a captured provider/model and immutable text, never a live Loop.

- [ ] Write tests for real user/final-assistant extraction, exclusion of tools/thinking, missing final answers, unchanged input, bounded context, tool-free request, model preservation, normalization and cancellation.
- [ ] Run `go test ./internal/promptsuggest ./internal/config` and observe the new tests fail before implementation.
- [ ] Implement extraction and generation using `Provider.Complete`, low effort, the bounds above and an explicit no-suggestion response contract.
- [ ] Add the boolean setting with default true, explicit false persistence and round-trip coverage.
- [ ] Run the scoped tests and `go test -race ./internal/promptsuggest`.

### Task 2: Terminal ghost text

**Files:** Create `internal/tui/prompt_suggestion.go` and `prompt_suggestion_test.go`; minimally wire `tui.go`, `tui_update.go`, `keybind_main.go`, session/model lifecycle and the configuration screen.

**Interfaces:** A UI-owned suggestion state tracks generation, session/model binding, cancellation and suggested text. Tea commands capture immutable input and return a typed result message. `acceptPromptSuggestion() bool` inserts text and moves the caret, but never submits.

- [ ] Add tests that a completed eligible turn schedules exactly one request; ineligible turns do not; visible text remains a placeholder with an empty real value.
- [ ] Add tests for Tab acceptance without submission, Escape/typing dismissal, completion priority, CJK layout, stale response rejection, model/session changes and settings disabled.
- [ ] Run the tests red, then implement the state machine and minimal lifecycle wiring.
- [ ] Preserve the normal placeholder when no suggestion exists and invalidate any asynchronous result on ownership/input changes.
- [ ] Run `go test ./internal/tui/...` and the scoped race tests.

### Task 3: Desktop endpoint and composer

**Files:** Create `internal/webui/prompt_suggestion.go`, `prompt_suggestion_test.go`, `prompt_suggestion_ui_test.go`, and `static/prompt-suggestions.js`; minimally wire `server.go`, `static/index.html`, `static/chat.js`, settings and existing styles.

**Interfaces:** `POST /api/prompt-suggestion` takes an explicit session ID and returns a suggestion only for an eligible completed session. Snapshot stored history and that session's authorized model route; return empty text on ordinary generation failure. The frontend owns an AbortController and monotonically increasing request generation.

- [ ] Add backend tests for session validation, completed-history extraction, correct provider/model selection, disabled/busy cases, cancellation and unchanged session data.
- [ ] Add browser/Node tests for empty-value ghost rendering, Tab acceptance without send, ordinary Tab behavior without a suggestion, Escape, IME, stale requests, queue/attachment conflicts and session/model switching.
- [ ] Run tests red, then implement a focused endpoint and composer module. Render provider text as plain text, never HTML.
- [ ] Trigger only from authoritative successful user-turn completion, not per-provider `turn_end`; invalidate on session/input/turn changes and disable.
- [ ] Run scoped Go and UI tests, including Desktop isolated-worker routing cases.

### Task 4: Integration, documentation and verification

**Files:** Update the relevant README/configuration documentation and `CHANGELOG.md` Unreleased section; add reproducible fixture coverage under the existing test harness where appropriate.

- [ ] Document Tab/Enter semantics, opt-out setting, extra short request, failure behavior and absence of automatic submission.
- [ ] Run formatting, root vet/build/full tests with bounded package parallelism, and relevant race tests.
- [ ] Build and attempt `make install` according to current project safeguards; do not overwrite a managed release if it refuses.
- [ ] Drive the actual CLI in tmux and the shared Desktop composer with a controlled local model. Verify gray text, Tab acceptance, editing, submission and session-switch races.
- [ ] Obtain one bounded independent review of the complete diff, fix verified issues, and rerun affected checks.
- [ ] Report source/build paths and exact verification results. Leave changes uncommitted unless the user asks to commit or release.
