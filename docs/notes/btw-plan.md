# Plan: `/btw` — ephemeral side questions (Claude Code-style)

Status: ready to implement. Self-contained: everything needed is in this
document plus the referenced files. Read §2 before writing any code.

## 1. Goal

Port Claude Code's `/btw` feature to crush: the user asks a quick "by the
way" question about the current session, a cheap model answers it using the
session's full conversation as context, and the answer appears in an
ephemeral overlay. Nothing is persisted: no user message, no assistant
message, no change to the session's history or context window. The main
agent run (if one is in flight) is not interrupted or affected.

Properties to preserve from the original:

- Works **while the agent is busy** — it is a read-only side channel.
- Full visibility into the session so far (user prompts, assistant text,
  tool calls and results).
- **No tool access** for the side model. It answers only from context.
- Ephemeral: close the overlay and it is gone. Zero trace in the DB.
- Cheap: uses the configured **small** model when it fits, falling back to
  the large model.

Naming: the user-facing name is **btw** (palette entry, HTTP route). Go
symbols use **SideQuestion** (clearer than an acronym in code).

## 2. Verified codebase facts

Verified against the tree at commit `e329a2ee` (branch `feat/rename-command`).
Line numbers WILL drift — treat them as anchors, re-locate each symbol with
grep before editing. Everything below was confirmed by reading the source;
do not re-derive it, but do re-find it.

### 2.1 One-shot, no-tools, no-persistence model calls already exist

- `sessionAgent.GenerateTitle` — `internal/agent/agent.go:1684` — is the
  template for "call a model without polluting the session":
  - Builds a fresh no-tools agent: `fantasy.NewAgent(m, fantasy.WithSystemPrompt(...), fantasy.WithMaxOutputTokens(tok), fantasy.WithUserAgent(userAgent))` (`agent.go:1706-1713`).
  - Tries **small model first, then large** via an attempts slice
    (`agent.go:1733-1760`).
  - Persists **no messages** (only updates the session title/usage row).
- `sessionAgent.Summarize` — `agent.go:1293` — is the template for "replay
  session history as provider messages":
  - `a.getSessionMessages(ctx, currentSession)` (`agent.go:1645`) →
    `a.messages.List(...)` then trims to after `SummaryMessageID` if set.
  - `a.preparePrompt(msgs, supportsImages)` (`agent.go:1486`) converts to
    `[]fantasy.Message`, strips images when unsupported, and **repairs
    orphaned tool calls/results** (`filterOrphanedToolResults`
    `agent.go:1582`, `syntheticToolResultsForOrphanedCalls` `agent.go:1618`).
    This matters for btw: a mid-run session has an unfinished tool call at
    the tail, and providers reject dangling tool calls.
  - Calls `agent.Stream(ctx, fantasy.AgentStreamCall{Prompt: text, Messages: aiMsgs, Headers: sessionHeaders(sessionID), ...})`.
  - BUT Summarize **persists** an assistant summary message — btw must NOT
    copy that part.
- `fantasy` (module `charm.land/fantasy`, v0.36.0) high-level API:
  - `fantasy.NewAgent(model, opts...) Agent`; `Agent.Generate(ctx, fantasy.AgentCall)` is the **non-streaming** one-shot call (`AgentCall{Prompt, Messages, Headers, MaxOutputTokens, ...}`).
  - Result text: `res.Response.Content.Text()` (pattern at `agent.go:1768`).
- Cache-affinity headers: `sessionHeaders(sessionID)` (`agent.go:1461`).
- Small/large models live privately on `sessionAgent`:
  `a.smallModel.Get()` / `a.largeModel.Get()` return
  `Model{Model fantasy.LanguageModel; CatwalkCfg catwalk.Model; ModelCfg config.SelectedModel; FlatRate bool}`
  (`agent.go:147-152`, reads at `agent.go:1702`). NOT exposed on the
  `Coordinator` interface — the new method must live inside
  `internal/agent` where these are reachable.
- System prompts are embedded templates: `//go:embed templates/title.md`
  (`agent.go:64`), `templates/summary.md` (`agent.go:67`).
- Message store writes are debounced (`internal/message/message.go:31-45`):
  readers that need the latest mid-stream state must call
  `messages.FlushAll(ctx)` before `List`. `Backend.ListSessionMessages`
  already does exactly this (`internal/backend/session.go:69-82`).

### 2.2 Server / backend / client plumbing

- Routes: stdlib `http.NewServeMux` with method+pattern routing,
  registered in `installHandler()` — `internal/server/server.go:145-220`.
  Session-scoped example:
  `mux.HandleFunc("POST /v1/workspaces/{id}/agent/sessions/{sid}/shell", ...)`.
- Handlers are methods on `controllerV1{backend, server}`
  (`internal/server/proto.go:16-19`). Canonical synchronous
  JSON-in/JSON-out handler to copy: `handlePostWorkspaceAgentSessionShell`
  (`proto.go:981-999`) — decode body, `req.SessionID = sid`, call backend,
  `jsonEncode(w, resp)`; decode failure →
  `jsonError(w, http.StatusBadRequest, "failed to decode request")`.
- Backend resolution: `b.GetWorkspace(id)` (`internal/backend/backend.go:209`),
  nil-check `ws.AgentCoordinator` → `ErrAgentNotInitialized`
  (pattern `internal/backend/agent.go:218-220`). `Workspace` embeds
  `*app.App`, so `ws.Sessions`, `ws.Messages`, `ws.AgentCoordinator` are
  promoted fields.
- Error mapping: `handleError(w, r, err)` (`proto.go:1133-1157`) maps
  sentinel errors → status (`ErrWorkspaceNotFound`→404,
  `ErrAgentNotInitialized`→400, default 500). Error body is
  `proto.Error{Message}` = `{"message": "..."}`.
- There is **no per-request streaming anywhere** — the only streaming
  endpoint is the global event SSE (`handleGetWorkspaceEvents`,
  `proto.go:258-314`). Decision (§3): btw is synchronous
  request/response; do not introduce per-request streaming.
- Client wrapper pattern: `internal/client/proto.go` — see `SendMessage`
  (`:421-439`) and `GetSession` (`:535-549`). POSTs need explicit
  `http.Header{"Content-Type": []string{"application/json"}}` (default is
  text/plain). Paths are relative — `sendReq` prefixes `/v1`. Error decode
  helper: `decodeErrorMessage` (`:446-452`).
- Proto types: `internal/proto/`, snake_case JSON tags, `,omitempty` for
  optional fields. Request bodies conventionally in
  `internal/proto/requests.go`.

### 2.3 TUI facts

- There is **no `/foo` text parsing** in the editor. Typing `/` on an
  **empty** textarea opens the command palette
  (`internal/ui/model/ui.go:2340-2343`); ctrl+p opens it too
  (`ui.go:2102-2106`). So "/btw" UX = open palette, type "btw", enter.
- Built-in palette commands: `defaultCommands()` in
  `internal/ui/dialog/commands.go:430-540`. Item shape:
  `NewCommandItem(t, id, title, shortcut, action)` with `.WithAliases()` /
  `.WithDescription()` (`internal/ui/dialog/commands_item.go:14-40`).
  Selecting an item returns its `Action` (any struct from
  `internal/ui/dialog/actions.go`).
- Action dispatch: `handleDialogMsg` switch in `ui.go:1586-1881`; generic
  cases `ActionClose` (`:1597`), `ActionCmd{Cmd}` (`:1617`),
  `ActionOpenDialog{DialogID}` (`:1628`). Dialog IDs map to constructors in
  `openDialog(id)` (`ui.go:3832-3872`).
- Dialog system: `internal/ui/dialog/dialog.go` — `Dialog` interface
  (`ID/HandleMsg/Draw`, `:35-44`), overlay stack with
  `OpenDialog`/`CloseDialog` (`:104,151`), optional `LoadingDialog`
  interface for spinners (`:47-50`).
- Templates to copy: `internal/ui/dialog/arguments.go` (textinput +
  viewport dialog), `internal/ui/dialog/permissions.go:387-435` (scrollable
  viewport sized from measured content + `common.Scrollbar`).
- Dialogs open and function while the agent is busy: key routing checks
  `m.dialog.HasDialogs()` first (`ui.go:2186-2189`). Busy-gating is a
  per-action choice; btw intentionally does NOT gate on busy.
- TUI never touches `internal/client.Client` directly — it calls
  `m.com.Workspace` (`internal/workspace/workspace.go:63-166`), which has
  two impls: `AppWorkspace` (in-process) and `ClientWorkspace` (HTTP).
  A new capability = one interface method + two impls.
- Async call pattern: wrap the workspace call in a `tea.Cmd` returning a
  custom msg (see `sendMessage`, `ui.go:3642-3689`). To learn how async
  results reach an open dialog, trace how the Commands dialog receives its
  async-loaded custom commands (`ui.go:586-611`, `commands.go:391-419`) and
  mirror that mechanism — do this before writing the dialog.
- Markdown rendering: `common.MarkdownRenderer(styles, width)`
  (`internal/ui/common/markdown.go:45-58`); NOT concurrency-safe — wrap
  with `common.LockMarkdownRenderer(r)` (usage at
  `internal/ui/chat/tools.go:1065-1069`).

### 2.4 Message → context facts

- `message.Message.ToAIMessage() []fantasy.Message`
  (`internal/message/content.go:499`) is exported and handles every part
  type incl. tool calls/results and shell commands. `preparePrompt` uses it
  per message.
- Child (task-tool) sessions store their messages under their own session
  ID; the parent history contains only the task tool's ToolCall/ToolResult
  pair, which is self-contained. **Do not recurse into child sessions.**
- Sessions track `PromptTokens`/`CompletionTokens` on the session row
  (`internal/session/session.go:50-63`) — use these for the model-fit
  check (§6.1); there are no per-message token counts.

## 3. Architecture decision

One new capability threaded through every existing layer, copying the
summarize plumbing shape at each hop:

```
TUI palette "btw" ──► dialog (input → spinner → answer viewport)
                          │ tea.Cmd
                          ▼
              workspace.Workspace.SideQuestion(ctx, sessionID, req)
              ├─ AppWorkspace  → app.AgentCoordinator.SideQuestion(...)
              └─ ClientWorkspace → client.SideQuestion(...)
                                        │ POST /v1/workspaces/{id}/sessions/{sid}/btw
                                        ▼
                          server handler → backend.SideQuestion(...)
                                        → ws.AgentCoordinator.SideQuestion(...)
                                        ▼
              sessionAgent.SideQuestion: FlushAll → List → preparePrompt
              → fantasy no-tools Agent.Generate (small, fallback large)
              → return text. PERSISTS NOTHING.
```

Decisions (do not relitigate):

1. **Synchronous, non-streaming.** No per-request streaming precedent
   exists (§2.2) and answers are short. UX is spinner-then-answer.
   `Agent.Generate`, not `Stream`.
2. **No tools.** Construct the fantasy agent without `WithTools`.
3. **Small model first, large fallback** — same attempts pattern as
   `GenerateTitle`, plus a context-window pre-check (§6.1).
4. **Zero persistence.** No `messages.Create`, no session `Save`, no
   usage/cost update in MVP (see §11 for the deliberate trade-off).
5. **Stateless server, multi-turn client.** Follow-up questions inside the
   open dialog are supported by sending prior Q/A pairs in the request
   (`exchanges`); the server appends them to the replayed history. No
   server-side session state for btw.
6. **Runs while busy.** No `IsSessionBusy` gate anywhere on this path.

## 4. API spec

### 4.1 Proto types — `internal/proto/requests.go`

```go
// SideQuestionExchange is one prior question/answer pair from the same
// ephemeral side conversation.
type SideQuestionExchange struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// SideQuestionRequest asks an ephemeral side question about a session.
type SideQuestionRequest struct {
	SessionID string                 `json:"session_id"`
	Question  string                 `json:"question"`
	Exchanges []SideQuestionExchange `json:"exchanges,omitempty"`
}

// SideQuestionResponse is the answer to a side question.
type SideQuestionResponse struct {
	Answer           string `json:"answer"`
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}
```

### 4.2 Route

`POST /v1/workspaces/{id}/sessions/{sid}/btw`

- 200: `SideQuestionResponse`.
- 400: empty/whitespace question, undecodable body, agent not initialized.
- 404: unknown workspace or session.
- 500: provider failure after both model attempts (body carries the
  redacted error message).

Register in `installHandler()` next to the other session routes. Handler
`handlePostWorkspaceSessionBTW` copies `handlePostWorkspaceAgentSessionShell`
verbatim in shape: decode → `req.SessionID = sid` → backend call →
`jsonEncode` / `c.handleError`.

### 4.3 Client — `internal/client/proto.go`

```go
func (c *Client) SideQuestion(ctx context.Context, id, sessionID string, req proto.SideQuestionRequest) (*proto.SideQuestionResponse, error)
```

POST to `fmt.Sprintf("/workspaces/%s/sessions/%s/btw", id, sessionID)` with
JSON content-type header; non-200 → `decodeErrorMessage`; decode
`proto.SideQuestionResponse`. Copy `GetSession`'s error wrapping style.

## 5. Agent layer — `internal/agent`

### 5.1 Interface additions

`Coordinator` (`internal/agent/coordinator.go:81-105`) and `SessionAgent`
(`agent.go:129-145`) each gain:

```go
SideQuestion(ctx context.Context, sessionID, question string, exchanges []SideQuestionExchange) (SideQuestionResult, error)
```

where (in `internal/agent`, so the agent package does not import proto):

```go
type SideQuestionExchange struct{ Question, Answer string }
type SideQuestionResult struct {
	Answer           string
	Model, Provider  string
	PromptTokens     int64
	CompletionTokens int64
}
```

The coordinator method delegates to `c.currentAgent` exactly like
`Summarize` (`coordinator.go:1166`). Backend/server convert between
`agent.SideQuestion*` and `proto.SideQuestion*` types.

### 5.2 `sessionAgent.SideQuestion` implementation

```go
func (a *sessionAgent) SideQuestion(ctx context.Context, sessionID, question string, exchanges []SideQuestionExchange) (SideQuestionResult, error)
```

Steps (each maps to an existing verified pattern):

1. Trim `question`; empty → error (surfaces as 400 via a sentinel or plain
   error mapped in the handler).
2. `session, err := a.sessions.Get(ctx, sessionID)`.
3. `a.messages.FlushAll(ctx)` then `msgs, err := a.getSessionMessages(ctx, session)`
   (history after last summary, same as Summarize).
4. Pick the model (§6.1) → `chosen Model`.
5. `aiMsgs, _ := a.preparePrompt(msgs, chosen.CatwalkCfg.SupportsImages)` —
   this gives orphan-repaired provider messages.
6. Append prior exchanges: for each exchange, append
   `fantasy.NewUserMessage(...)` with the question and an assistant message
   with the answer (use the same constructors `ToAIMessage`/`preparePrompt`
   output uses — grep fantasy's message constructors, e.g.
   `fantasy.NewUserMessage` / assistant equivalent, before writing this).
7. Build the agent:
   ```go
   sideAgent := fantasy.NewAgent(chosen.Model,
       fantasy.WithSystemPrompt(string(sideQuestionPrompt)),
       fantasy.WithMaxOutputTokens(2048),
       fantasy.WithUserAgent(userAgent))
   res, err := sideAgent.Generate(ctx, fantasy.AgentCall{
       Prompt:   question,
       Messages: aiMsgs,
       Headers:  sessionHeaders(sessionID),
   })
   ```
8. On error with the small model, retry once with the large model
   (attempts loop copied from `GenerateTitle`, `agent.go:1733-1760`). If
   both fail, return the last error.
9. Return `SideQuestionResult{Answer: res.Response.Content.Text(), Model: chosen..., PromptTokens: res.TotalUsage..., ...}`
   — grep `TotalUsage` field names in fantasy before writing.
10. **No Create/Update/Save calls anywhere in this function.**

### 5.3 System prompt — `internal/agent/templates/btw.md` (new embed)

```markdown
You are answering a quick side question about the conversation shown above.
The conversation is between a user and a coding agent working in a project.

Rules:
- Answer ONLY from information already present in the conversation. You
  have no tools: you cannot read files, run commands, or search.
- If the conversation does not contain enough information to answer, say
  so plainly and say what is missing. Do not guess.
- Be concise. Prefer a short direct answer over structure and headers.
- Do not take or suggest taking actions in the session; you are a read-only
  side channel.
```

Embed next to the others: `//go:embed templates/btw.md` →
`var sideQuestionPrompt []byte` (`agent.go` top, near `:64-67`).

## 6. Model selection & size guard

### 6.1 Fit check

Before step 7 in §5.2:

```go
small := a.smallModel.Get()
large := a.largeModel.Get()
chosen := small
used := session.PromptTokens + session.CompletionTokens
if cw := int64(small.CatwalkCfg.ContextWindow); cw > 0 && used > (cw*8)/10 {
    chosen = large // history very likely won't fit the small model
}
```

Session token counters are the only cheap size signal (§2.4); 80% leaves
headroom for the system prompt, question, and counter drift. If the large
model then also errors with a context overflow, return the error as-is —
the handler maps it to 500 and the dialog shows it; the user's remedy is
`/summarize`. **No custom history truncation in MVP** (silent truncation
produces confidently wrong answers, worse than an error).

### 6.2 Attempts order

- If fit check chose small: attempts = [small, large].
- If fit check chose large: attempts = [large] only.
- Skip an attempt whose `Model` is nil (mirrors GenerateTitle's guards —
  check how it nil-guards before copying).

## 7. Backend + server + workspace layers

### 7.1 `internal/backend/agent.go`

```go
func (b *Backend) SideQuestion(ctx context.Context, workspaceID string, req proto.SideQuestionRequest) (*proto.SideQuestionResponse, error)
```

Copy `SummarizeSession` (`backend/agent.go:212-223`) shape: `GetWorkspace`
→ nil-check `ws.AgentCoordinator` → convert `req.Exchanges` to
`[]agent.SideQuestionExchange` → call
`ws.AgentCoordinator.SideQuestion(ctx, req.SessionID, req.Question, exchanges)`
→ convert result to proto. Validate `req.Question` non-empty here too
(defense in depth; return a 400-mapped sentinel, add it to `handleError`'s
400 list if a new sentinel is created).

### 7.2 `internal/workspace`

Add to the `Workspace` interface (`workspace.go:63-166`):

```go
SideQuestion(ctx context.Context, sessionID, question string, exchanges []proto.SideQuestionExchange) (*proto.SideQuestionResponse, error)
```

- `AppWorkspace`: call the app's coordinator directly (convert types), or —
  simpler and consistent with how other AppWorkspace methods work — check
  first how `AppWorkspace` implements its summarize/shell equivalents and
  mirror that exact route (some go through app, not backend).
- `ClientWorkspace`: `w.client.SideQuestion(ctx, w.id, sessionID, proto.SideQuestionRequest{...})`.

Use proto types at this interface (the workspace layer already speaks
proto — verify with one existing method before deciding otherwise).

## 8. TUI — palette entry + dialog

### 8.1 Palette

In `defaultCommands()` (`commands.go:430-540`), gated on `hasSession`:

```go
NewCommandItem(t, "btw", "Btw — ask a side question", "",
    ActionOpenDialog{DialogID: BtwID}).
    WithAliases("sidequestion", "ask", "question").
    WithDescription("Ask about this session without touching its history")
```

Add `BtwID = "btw"` const and a case in `openDialog` (`ui.go:3832-3872`)
constructing the dialog with `m.com` and the current session ID. Because
`/` on an empty editor opens the palette, the muscle-memory flow
`/` → `btw` → enter works with zero editor changes.

### 8.2 Dialog — new `internal/ui/dialog/btw.go`

States: **input → loading → answer**, looping back to input for
follow-ups. Skeleton:

- Fields: `question textinput.Model`, `viewport viewport.Model`,
  `exchanges []proto.SideQuestionExchange`, `state`, `spinner` (implement
  `LoadingDialog`), `sessionID`, `com`, width/height bookkeeping copied
  from `arguments.go`.
- **input**: single textinput, title "btw", help line
  `enter ask · esc close`. Enter with non-empty text → state=loading,
  return `ActionCmd{Cmd: askCmd}` where `askCmd` is a `tea.Cmd` calling
  `com.Workspace.SideQuestion(ctx, sessionID, q, d.exchanges)` and
  returning a `SideQuestionResultMsg{Answer string; Err error}` (define the
  msg in the dialog package). Before implementing, trace the Commands
  dialog's async-load round trip (§2.3) so the result msg actually reaches
  `HandleMsg` — replicate that routing, including any needed case in
  `handleDialogMsg`.
- **loading**: spinner via the `LoadingDialog` mechanism
  (`dialog.go:47-50`; see how Commands uses StartLoading/StopLoading).
  Esc cancels: close dialog; the in-flight HTTP call is dropped (pass a
  cancellable context if the traced pattern supports it; otherwise let it
  finish and discard the msg for a closed dialog — the overlay routes msgs
  only to open dialogs, verify which happens).
- **answer**: render `answer` through
  `common.MarkdownRenderer(styles, viewportWidth)` under
  `LockMarkdownRenderer` (§2.3), set into the viewport, scrollable with
  `common.Scrollbar` (copy `permissions.go:387-435` sizing). Append
  `{q, answer}` to `d.exchanges`. Help line:
  `↑/↓ scroll · enter ask follow-up · esc close`. Enter → back to input
  state (keeping exchanges); the next ask sends the accumulated exchanges.
  Error result → show the error text (styled as an error, no markdown
  render) with the same keys.
- On close, everything is garbage — that's the feature.

Sizing: modest overlay, e.g. width `min(80, screenWidth-4)`, follow
`arguments.go` conventions rather than inventing new ones.

## 9. Telegram bridge (conditional milestone)

Only applies when working on a branch that contains `feat/telegram-bridge`
(`internal/telegram/` exists). Add a `/btw <question>` command to the
bridge:

- `cmdBtw(ctx, text string)`: requires an active session (same guard/reply
  as other session commands); calls
  `Client.SideQuestion(ctx, wsID, activeSession, proto.SideQuestionRequest{Question: text})`;
  sends the answer via the existing chunked-send path (`formatChunks` /
  `sendHTML` fallback rules). No exchanges/follow-up state in the bridge —
  each `/btw` is standalone. Register in the command dispatch and in the
  `/help` text. Errors → plain-text reply, never containing the bot token
  (client errors are server-side messages, safe, but keep the redaction
  habit if any transport error is surfaced).

## 10. Testing

Run `go test ./...` without `-race` locally (unsupported on the dev
device); note in the PR that CI must run `-race`. gofumpt via
`go run mvdan.cc/gofumpt@latest -l -w` on touched files. Conventions:
testify `require`, `t.Parallel()` where safe, snake_case JSON tags.

1. **Proto round-trip** (`internal/proto`): marshal/unmarshal
   `SideQuestionRequest`/`Response`, assert exact JSON keys
   (`session_id`, `exchanges` omitted when empty).
2. **Client method** (`internal/client`): follow the existing client test
   pattern (grep `httptest` in `internal/client`; if none exists, mirror
   `internal/telegram`'s fake-server style): fake server asserting method,
   path `/v1/workspaces/ws1/sessions/s1/btw`, body fields; returns a
   canned response; also a 400 case asserting `decodeErrorMessage` text
   surfaces.
3. **Agent unit tests** (`internal/agent`): check how existing tests fake
   `fantasy.LanguageModel` (grep `_test.go` in `internal/agent` for a mock
   or fake provider). If a usable fake exists: test that (a) a
   `SideQuestion` call performs zero `messages.Create` calls (fake the
   message service or assert message count unchanged), (b) exchanges are
   appended after history, (c) small→large fallback triggers on error,
   (d) the 80% fit check routes to large. If no fake exists, extract the
   pure parts — the fit check and the attempts-order function — into
   standalone funcs and unit-test those; do not build a provider-mocking
   framework for this feature.
4. **Handler test** (`internal/server`): only if the package already has
   handler tests to pattern-match (grep first). Otherwise cover via the
   client test's fake and manual verification.
5. **Manual test script** (document in the PR):
   `crush server` + TUI attached; mid-run, open palette → btw → ask
   "which files have we changed so far?"; verify answer, verify
   `message_count` unchanged after (via `/sessions` or DB), verify main
   run unaffected; repeat in local (non-client) mode; oversized session →
   confirm graceful error, `/summarize`, retry works.

## 11. Constraints & non-goals

- **Zero persistence** is the contract. If any code path in the new
  feature writes to `sessions` or `messages`, it is a bug. Consequence
  (accepted): btw token spend is NOT reflected in the session's cost —
  the response reports its own token usage, and the TUI dialog may show it
  in the help/footer line. Do not "fix" this by writing to the session.
- **No tools, ever** — including no `PrepareStep` that injects tools.
- **No YOLO/permission interaction**: the side model can't act, so the
  permission system is untouched. Keep it that way.
- **No new dependencies.**
- Non-goals for this iteration: token streaming into the dialog, btw
  history across dialog opens, recursing into task-tool child sessions,
  a config knob for the btw model, cost attribution.

## 12. Milestones

- **M1 — agent core**: `templates/btw.md`, `SideQuestion` on
  `sessionAgent` + `Coordinator`, fit check + attempts, agent-layer tests
  (§10.3). Compiles standalone.
- **M2 — API**: proto types, backend method, route + handler, client
  method, proto + client tests (§10.1, §10.2).
- **M3 — workspace**: interface method + both impls.
- **M4 — TUI**: palette item, `btw.go` dialog (input→loading→answer,
  follow-ups), manual test pass (§10.5).
- **M5 — telegram** (only on a telegram-bridge branch): `/btw` command +
  help text.
- **M6 — polish**: docs note (`docs/notes/btw.md`, short user-facing:
  what it is, how to open, follow-ups, "answers only from session
  context"), gofumpt sweep, full `go test ./...`.

One commit per milestone, semantic one-liners, e.g.
`feat(agent): add ephemeral side-question completion`,
`feat(ui): add btw side-question dialog`.

## 13. Pitfalls checklist (review against this)

1. ❑ No `messages.Create/Update`, no `sessions.Save/UpdateTitleAndUsage`
   anywhere in the SideQuestion path (compare: Summarize persists —
   copying it too literally reintroduces writes).
2. ❑ `messages.FlushAll` before `List` — debounced writes mean a mid-run
   session otherwise returns stale history (§2.1).
3. ❑ Orphaned tool-call repair actually runs (use `preparePrompt`, don't
   hand-roll message conversion) — a busy session's dangling tool call
   will otherwise 400 at the provider.
4. ❑ History replay starts after `SummaryMessageID` (comes free with
   `getSessionMessages` — don't substitute a raw `messages.List`).
5. ❑ No tools passed to `fantasy.NewAgent`; MaxOutputTokens set.
6. ❑ Small→large fallback and the 80% fit check both present; nil-model
   attempts skipped.
7. ❑ POST client call sets `Content-Type: application/json` explicitly
   (client default is text/plain — silent 400s otherwise).
8. ❑ Handler validates empty question → 400, not a provider round-trip.
9. ❑ Dialog result-msg routing verified against the Commands dialog's
   async pattern BEFORE implementation (msgs must reach `HandleMsg`).
10. ❑ Markdown renderer used under `LockMarkdownRenderer` (not
    concurrency-safe).
11. ❑ btw palette item gated on `hasSession`, NOT gated on agent busy.
12. ❑ Exchanges appended AFTER session history (order: history, prior
    Q/A pairs, then the new question as Prompt).
13. ❑ Session-not-found and workspace-not-found map to 404 via existing
    sentinels; no new 500s for user errors.
14. ❑ Line numbers in §2 re-located with grep, not trusted blindly.
