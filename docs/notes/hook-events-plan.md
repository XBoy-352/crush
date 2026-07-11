# Plan: hook events — add UserPromptSubmit, PostToolUse, and Stop

Status: ready to implement. Self-contained: everything needed is in this
document plus the referenced files. Read §2 before writing any code.

## 1. Goal

Extend the hooks system from `PreToolUse`-only to three more lifecycle
events, keeping the existing JSON protocol, aggregation rules, and
Claude Code-compatible payload conventions:

- **UserPromptSubmit** — fires after the user submits a prompt, before the
  turn reaches the LLM. Hooks can deny the submission (with a visible
  reason), rewrite the prompt (`updated_prompt`), or inject context. This
  implements what `docs/hooks/FUTURE.md` already promises.
- **PostToolUse** — fires after a tool call completes, with the tool's
  response in the payload. Hooks can inject context or halt the turn; they
  cannot un-run the tool.
- **Stop** — fires when a top-level agent turn ends (complete, error, or
  cancelled). Observational only: audit/notify use cases.

Properties to preserve (review criteria):

- `PreToolUse` behavior is byte-for-byte unchanged: same payload, same
  envelope, same aggregation, same exit-code semantics; all existing tests
  in `internal/hooks` and `internal/agent/hooked_tool_test.go` pass
  unmodified.
- All new events fire ONLY for the top-level agent, never for subagents —
  same scoping rule PreToolUse already has.
- Hook execution failures never break the turn: everything degrades to
  "no opinion" exactly like today (`runner.go` error handling reused as-is).
- Config keys are case/underscore-insensitive like `PreToolUse` is today
  (`pre_tool_use` works), via `normalizeHookEvent`.
- A config with only `PreToolUse` hooks builds zero new runners and takes
  zero new subprocess spawns per turn.

Naming: event constants `EventUserPromptSubmit = "UserPromptSubmit"`,
`EventPostToolUse = "PostToolUse"`, `EventStop = "Stop"`. New runner entry
point `RunEvent` + input struct `EventInput`. Envelope field
`updated_prompt`.

## 2. Verified codebase facts

Verified against the tree at commit `df2b2fa5` (branch `feat/btw`). Line
numbers WILL drift — treat them as anchors, re-locate each symbol with grep
before editing.

### Hook engine (`internal/hooks/`)

- `EventPreToolUse = "PreToolUse"` is the only event constant
  (`hooks.go:14-16`). `HaltExitCode = 49` (`hooks.go:22`).
- `HookResult` (`hooks.go:69-75`): `Decision, Halt, Reason, Context,
  UpdatedInput`. `AggregateResult` (`hooks.go:78-86`) adds `HookCount`,
  `Hooks []HookInfo`.
- `aggregate(results, origToolInput)` (`hooks.go:94-158`): deny > allow >
  none; halt sticky; reasons/contexts newline-joined in config order;
  `updated_input` patches shallow-merged sequentially. New `UpdatedPrompt`
  aggregation (last writer wins) slots into this loop.
- `Runner` (`runner.go:36-40`): `{hooks []compiledHook, cwd, projectDir}`.
  `NewRunner(hooks, cwd, projectDir)` compiles matchers (`runner.go:50-74`);
  empty matcher = match everything, non-matching regex hooks are skipped per
  tool name (`matchingHooks`, tested against `toolName` only).
- `Runner.Run(ctx, eventName, sessionID, toolName, toolInputJSON)`
  (`runner.go:90-142`): filters by matcher, dedups by command string, builds
  env+payload once, runs all hooks in parallel goroutines, aggregates in
  config order, fills `agg.Hooks`. **It already takes an arbitrary event
  name** — the engine is event-agnostic today.
- `runOne` (`runner.go:172+`): 30s default timeout
  (`HookConfig.TimeoutDuration`, `config.go:610-615`), executes via the
  package var `runShell = shell.Run` (`runner.go:26` — swappable in tests),
  abandon-on-timeout grace of 1s (`runner.go:20`). Exit codes: 0 → parse
  stdout JSON; 2 → deny with stderr reason; 49 → deny+halt; other →
  non-blocking (`runner.go:220-255` per docs — re-locate in `runOne`).
- `Payload` (`input.go:23-29`): `event`, `session_id`, `cwd`, `tool_name`,
  `tool_input` (raw JSON object). `BuildPayload(eventName, sessionID, cwd,
  toolName, toolInputJSON)` (`input.go:32-49`). `BuildEnv` (`input.go:53-76`)
  sets `CRUSH_EVENT`, `CRUSH_TOOL_NAME`, `CRUSH_SESSION_ID`, `CRUSH_CWD`,
  `CRUSH_PROJECT_DIR`, plus `CRUSH_TOOL_INPUT_COMMAND`/`_FILE_PATH`
  extracted from tool input via gjson.
- `parseStdout` (`input.go:80-124`): native envelope `{version, decision,
  halt, reason, context, updated_input}`; Claude Code compat via
  `hookSpecificOutput` (`input.go:92-94`, `parseClaudeCodeOutput` 159-182);
  `parseDecision` case-insensitive (`input.go:202-211`); `parseContext`
  accepts string or []string (`input.go:128-155`).

### Wiring (`internal/agent/`)

- `hookedTool` (`hooked_tool.go:18-25`) wraps `fantasy.AgentTool`; built by
  `wrapToolsWithHooks(tools, runner, isSubAgent)` (`hooked_tool.go:31-40`)
  which returns tools **unchanged when runner is nil or isSubAgent**.
- `hookedTool.Run` (`hooked_tool.go:54-100`): fires PreToolUse; on
  deny/halt returns `fantasy.NewTextErrorResponse(reason)` with
  `resp.StopTurn = result.Halt` and the inner tool never runs; on
  `UpdatedInput` rewrites `call.Input`; on allow stamps
  `permission.WithHookApproval(ctx, call.ID)`; then `h.inner.Run(ctx, call)`
  — **`resp` exists from line 86 onward: that is the PostToolUse fire
  site**. Context is appended to `resp.Content` (91-96); metadata merged
  under JSON key `"hook"` via `mergeHookMetadata` (124-142, uses
  `sjson.SetRaw(existing, "hook", ...)`).
- Runner construction: `buildTools` builds ONE runner only if PreToolUse
  hooks exist (`coordinator.go:652-656`), passing
  `cwd = projectDir = c.cfg.WorkingDir()`; wraps at `coordinator.go:730`
  (grep `wrapToolsWithHooks`).
- Coordinator run entry: `coordinator.run(ctx, accept, sessionID, prompt,
  attachments...)` (`coordinator.go:207`) is the single top-level entry
  behind `Run`/`RunAccepted` (192-200). It starts with
  `c.readyWg.Wait()` (208-210) then `UpdateModels`. `prompt` is in scope as
  a plain string; the call is forwarded to
  `c.currentAgent.Run(ctx, SessionAgentCall{... Prompt: prompt ...})`
  (272-286). **Subagent runs do NOT pass through coordinator.run** — grep
  `runSubAgent` in `internal/agent/agent_tool.go` to confirm before relying
  on this; the UserPromptSubmit fire site is here precisely because it is
  top-level-only by construction.
- Turn completion: `sessionAgent.Run` installs a deferred block
  (`agent.go:764-796`) that flushes messages and builds
  `notify.RunComplete{SessionID, RunID, MessageID, Text, Error, Cancelled}`
  then calls `a.publishRunComplete(ctx, call, complete)` (795). The
  early-cancel path also calls `a.publishRunComplete` (`agent.go:622,625`).
  **`publishRunComplete` is the single choke point for Stop** — grep
  `func (a *sessionAgent) publishRunComplete` for its body.
- `sessionAgent` has an `isSubAgent`-style flag via
  `SessionAgentOptions.IsSubAgent` (`coordinator.go:593`); grep
  `IsSubAgent` in `agent.go` for the stored field name.
- Session id is put on the run context at `agent.go:642` / `agent.go:725`
  (`tools.SessionIDContextKey`).

### Config (`internal/config/`)

- `HookConfig` (`config.go:588-597`): `{Name, Matcher, Command, Timeout}`,
  pure data. `Hooks map[string][]HookConfig` on `Config` (`config.go:640`).
- `ValidateHooks` (`load.go:1259`) re-keys the map through
  `normalizeHookEvent` (`load.go:1245`): lowercases, strips underscores;
  **only `"pretooluse"` maps to canonical today; unknown keys pass through
  unchanged** — new events MUST be added to this switch.
- Scope merge: configs merge global-first then project
  (`load.go:868,923`), and **arrays concatenate** — `Hooks["X"]` = global
  hooks ++ project hooks, all run, config order = global first.

### Docs & skill surfaces (must be updated together)

- `docs/hooks/README.md` — canonical spec; Reference section is already
  split into common vs per-event fields.
- `docs/hooks/FUTURE.md` — contains the agreed `UserPromptSubmit` design
  (payload with `prompt`+`attachments`, `updated_prompt` full-replacement
  last-writer-wins, deny blocks the submission, `allow` == silence, no
  permission-bypass semantics). This plan implements it; delete that
  section once shipped. `context_files` and sub-agent opt-in remain future.
- `internal/skills/builtin/crush-hooks/SKILL.md` — line 19 says "only
  PreToolUse"; update supported events, payload table, and per-event fields.

### Tests

- `internal/hooks/hooks_test.go`: `TestAggregation` drives `aggregate()`
  directly; `TestParseStdout` covers envelope forms; `TestRunnerExitCode*`
  substitute the `runShell` package var with a fake. Imitate these shapes.
- `internal/agent/hooked_tool_test.go`: `fakeTool` records invocation ctx;
  tests wrap/passthrough/deny/allow-stamp. Read it before writing new tests
  and reuse its helpers; note it lives in package `agent`, so it cannot
  swap `hooks.runShell` — existing tests build real `hooks.NewRunner`s with
  trivial shell commands (verify by reading the file first).

### What does NOT exist (do not invent)

- No `RunEvent`/`EventInput`; no `updated_prompt` parsing; no post-tool or
  stop call sites; no per-event runner registry; no hook events fired from
  the TUI layer. There is no async/background hook mode — do not add one.

## 3. Architecture decision

```
coordinator.run ──(1) UserPromptSubmit──► hooks.Runner.RunEvent
   │   deny/halt → return error, turn never starts
   │   updated_prompt → replace prompt   context → append to prompt
   ▼
sessionAgent.Run ──► agent.Stream
   │                    │ tool call
   │                    ▼
   │            hookedTool.Run
   │              ├─(2) PreToolUse   (unchanged)
   │              ├─ inner.Run → resp
   │              └─(3) PostToolUse  (context→resp, halt→StopTurn)
   ▼
publishRunComplete ──(4) Stop──► fire-and-log (no control flow)
```

Decisions (do not relitigate):

1. **One `Runner` per event, built at the call site from
   `cfg.Hooks[event]`** — same pattern as the existing PreToolUse runner in
   `buildTools`. No shared registry, no caching layer; `NewRunner` just
   compiles a few regexes. Losing option — a `Runners` map on the
   coordinator: lost because config reloads would need invalidation
   machinery the per-call-site build gets for free.
2. **New entry point `RunEvent(ctx, EventInput)`; existing `Run` becomes a
   thin wrapper** delegating to it. Signature-extending `Run` for every
   event-specific field was the losing option (unbounded parameter growth).
3. **UserPromptSubmit fires in `coordinator.run`, immediately after
   `readyWg.Wait()`** — top-level-only by construction, covers TUI, server,
   and `crush run` uniformly, and a denied prompt aborts before any side
   effect (no user message row, no model refresh). FUTURE.md offered
   `sessionAgent.Run` as the alternative; lost because sessionAgent also
   serves subagents and queued-recursion paths that would need gating.
4. **A denied prompt returns an error** (`prompt blocked by hook: <reason>`)
   from `coordinator.run`; no message is persisted. The rewritten prompt
   (when `updated_prompt` fires) IS what gets stored and displayed — one
   prompt, the one the model saw. Losing option (FUTURE.md open question) —
   store original + rewritten: lost because it needs a DB migration and UI
   affordance for marginal value; revisit on user demand. Record in docs.
5. **Hook `context` for UserPromptSubmit is appended below the prompt** as
   `\n\n<hook-context>\n...\n</hook-context>` — the user's words stay first.
6. **PostToolUse fires inside `hookedTool.Run` after `inner.Run`**, not in
   the fantasy `OnToolResult` callback — identical scoping to PreToolUse
   (top-level tools only, skipped when pre-hook denied) with the response
   in scope. Effects: `context` appends to `resp.Content` (reusing the
   existing append at `hooked_tool.go:91-96`); `halt` sets
   `resp.StopTurn = true`; `deny` appends `"\n[post-hook] <reason>"` to
   content (model-visible nudge) without setting `IsError`;
   `updated_input`/`updated_prompt` are ignored with a `slog.Warn`.
   PostToolUse does NOT fire when `inner.Run` returns a Go error (transport
   failure — there is no response to report).
7. **Stop fires inside `publishRunComplete`**, gated on `!isSubAgent`,
   covering all three publish call sites with one insertion. Stop is
   observational: decisions, halt, context, and rewrites from Stop hooks
   are ignored (logged at debug). Blocking-stop semantics are a non-goal.
8. **Matchers for non-tool events**: mechanics unchanged — the matcher is
   still tested against `ToolName`, which is `""` for UserPromptSubmit and
   Stop, so any non-empty matcher simply never fires. Documented: "leave
   `matcher` empty for non-tool events."
9. **No new env vars** beyond the existing set (`CRUSH_EVENT` already
   distinguishes events). The prompt goes only in the stdin payload — env
   vars have size limits and leak into child processes.
10. **Payload field names follow Claude Code where it has them**:
    `prompt` (UserPromptSubmit), `tool_response` (PostToolUse). Crush keeps
    its native `event` key (already divergent from Claude Code's
    `hook_event_name`; do not change it now).

## 4. API / type spec

`internal/hooks/hooks.go` — constants and result fields:

```go
const (
	EventPreToolUse       = "PreToolUse"
	EventUserPromptSubmit = "UserPromptSubmit"
	EventPostToolUse      = "PostToolUse"
	EventStop             = "Stop"
)

// KnownEvents lists every event this build fires, for config validation.
func KnownEvents() []string {
	return []string{EventPreToolUse, EventUserPromptSubmit, EventPostToolUse, EventStop}
}
```

Add to `HookResult`: `UpdatedPrompt string` (parsed from `updated_prompt`,
full replacement). Add to `AggregateResult`: `UpdatedPrompt string`
(last writer in config order wins; empty when no hook set it).

`internal/hooks/input.go`:

```go
// ToolResponsePayload describes a completed tool call for PostToolUse.
type ToolResponsePayload struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

type Payload struct {
	Event        string               `json:"event"`
	SessionID    string               `json:"session_id"`
	CWD          string               `json:"cwd"`
	ToolName     string               `json:"tool_name,omitempty"`
	ToolInput    json.RawMessage      `json:"tool_input,omitempty"`
	Prompt       string               `json:"prompt,omitempty"`        // UserPromptSubmit
	ToolResponse *ToolResponsePayload `json:"tool_response,omitempty"` // PostToolUse
	Outcome      string               `json:"outcome,omitempty"`       // Stop: complete|cancelled|error
	Error        string               `json:"error,omitempty"`         // Stop
}
```

(`omitempty` on `tool_name`/`tool_input` is safe: PreToolUse always sets
both, so its emitted payload is unchanged.)

`internal/hooks/runner.go`:

```go
// EventInput carries everything needed to fire one hook event.
type EventInput struct {
	Event        string
	SessionID    string
	ToolName     string // tool events only; "" otherwise
	ToolInput    string // raw JSON; tool events only
	Prompt       string
	ToolResponse *ToolResponsePayload
	Outcome      string
	Error        string
}

func (r *Runner) RunEvent(ctx context.Context, in EventInput) (AggregateResult, error)

// Run is the legacy PreToolUse-shaped entry point; now a wrapper:
func (r *Runner) Run(ctx context.Context, eventName, sessionID, toolName, toolInputJSON string) (AggregateResult, error) {
	return r.RunEvent(ctx, EventInput{Event: eventName, SessionID: sessionID, ToolName: toolName, ToolInput: toolInputJSON})
}
```

Native envelope gains one field, all events: `"updated_prompt": "..."`
(string; non-string → ignored with warn). Only UserPromptSubmit consumes it.

`internal/agent/hooked_tool.go`:

```go
type hookedTool struct {
	inner fantasy.AgentTool
	pre   *hooks.Runner // nil = no PreToolUse hooks
	post  *hooks.Runner // nil = no PostToolUse hooks
}

func wrapToolsWithHooks(tools []fantasy.AgentTool, pre, post *hooks.Runner, isSubAgent bool) []fantasy.AgentTool
// returns tools unchanged when (pre == nil && post == nil) || isSubAgent
```

`internal/agent/agent.go` — `SessionAgentOptions` gains:

```go
	// StopHooks fires the Stop event when a top-level turn ends. Nil for
	// subagents or when no Stop hooks are configured.
	StopHooks *hooks.Runner
```

## 5. Hooks package changes

1. Add constants + `KnownEvents` (§4) to `hooks.go`.
2. `HookResult.UpdatedPrompt` / `AggregateResult.UpdatedPrompt`; in
   `aggregate` (`hooks.go:94-158`) add inside the loop:
   `if r.UpdatedPrompt != "" { updatedPrompt = r.UpdatedPrompt }` (last
   writer wins — deliberately NOT the shallow-merge used for
   `updated_input`, per FUTURE.md: a prompt is one string with no key
   structure).
3. `parseStdout` (`input.go:96-103` struct): add
   `UpdatedPrompt string \`json:"updated_prompt"\`` and copy it into the
   result. Do not add it to the Claude Code `hookSpecificOutput` path.
4. `input.go`: add `ToolResponsePayload`, extend `Payload` (§4), and add
   `BuildEventPayload(in EventInput, cwd string) []byte` that fills all
   fields (tool_input validity fallback to `{}` only when `in.ToolInput`
   is non-empty — copy the existing check at `input.go:33-36`). Keep
   `BuildPayload` as a wrapper calling it, so existing tests still pass.
5. `runner.go`: implement `RunEvent` by renaming the body of `Run` and
   switching it to `EventInput` (matcher filter uses `in.ToolName`; env via
   existing `BuildEnv(in.Event, in.ToolName, in.SessionID, r.cwd,
   r.projectDir, in.ToolInput)`; payload via `BuildEventPayload`). `Run`
   becomes the §4 wrapper. `aggregate` call site passes `in.ToolInput`
   unchanged.

## 6. Config validation

1. `normalizeHookEvent` (`load.go:1245`): add cases
   `"userpromptsubmit" → EventUserPromptSubmit`,
   `"posttooluse" → EventPostToolUse`, `"stop" → EventStop`. Use the
   `hooks` package constants if the import direction allows; if
   `internal/config` cannot import `internal/hooks` (grep both packages'
   imports first — hooks imports config, so config must NOT import hooks),
   use string literals and add a comment pointing at `hooks.KnownEvents`.
2. In `ValidateHooks` (`load.go:1259`), after normalization, `slog.Warn`
   for any key not in the known set (do not error — forward compat with
   configs written for newer builds).

## 7. UserPromptSubmit call site (`coordinator.go`)

In `coordinator.run` (`coordinator.go:207`), immediately after the
`readyWg.Wait()` block (208-210):

```go
	if promptHooks := c.cfg.Config().Hooks[hooks.EventUserPromptSubmit]; len(promptHooks) > 0 {
		runner := hooks.NewRunner(promptHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
		agg, err := runner.RunEvent(ctx, hooks.EventInput{
			Event:     hooks.EventUserPromptSubmit,
			SessionID: sessionID,
			Prompt:    prompt,
		})
		if err != nil {
			slog.Warn("UserPromptSubmit hook error, proceeding", "error", err)
		}
		if agg.Decision == hooks.DecisionDeny || agg.Halt {
			reason := cmp.Or(agg.Reason, "blocked by hook")
			return nil, fmt.Errorf("prompt blocked by hook: %s", reason)
		}
		if agg.UpdatedPrompt != "" {
			prompt = agg.UpdatedPrompt
		}
		if agg.Context != "" {
			prompt += "\n\n<hook-context>\n" + agg.Context + "\n</hook-context>"
		}
	}
```

Notes for the executor: `cmp` is already imported in coordinator.go (grep);
the runner construction copies `coordinator.go:652-656`. Attachments are not
passed in the payload in v1 (FUTURE.md lists them; descoped — see §11).
Slash commands and MCP prompts are expanded before reaching
`coordinator.run`, so hooks see the final prompt text — verify by tracing
one palette command before writing docs, and state whatever you find in
README.md.

## 8. PostToolUse (`hooked_tool.go` + `coordinator.go`)

1. Restructure `hookedTool` to hold `pre`/`post` (§4). In `Run`
   (`hooked_tool.go:54`): the existing body guards with
   `if h.pre != nil { ... }` around the PreToolUse block (a post-only
   config must still work); after the successful `h.inner.Run` (line 86),
   fire:

```go
	if h.post != nil {
		postAgg, perr := h.post.RunEvent(ctx, hooks.EventInput{
			Event:     hooks.EventPostToolUse,
			SessionID: sessionID,
			ToolName:  call.Name,
			ToolInput: call.Input, // post-rewrite input: what actually ran
			ToolResponse: &hooks.ToolResponsePayload{
				Content: resp.Content,
				IsError: resp.IsError,
			},
		})
		if perr != nil {
			slog.Warn("PostToolUse hook error, ignoring", "tool", call.Name, "error", perr)
		}
		if postAgg.Context != "" {
			if resp.Content != "" {
				resp.Content += "\n"
			}
			resp.Content += postAgg.Context
		}
		if postAgg.Decision == hooks.DecisionDeny && postAgg.Reason != "" {
			resp.Content += "\n[post-hook] " + postAgg.Reason
		}
		if postAgg.Halt {
			resp.StopTurn = true
		}
		resp.Metadata = mergeHookMetadataKey(resp.Metadata, postAgg, "hook_post")
	}
```

2. Generalize `mergeHookMetadata` (`hooked_tool.go:125-142`) to
   `mergeHookMetadataKey(existing string, result hooks.AggregateResult, key string)`
   (the current function passes `"hook"`; keep a wrapper so the pre path is
   untouched).
3. `coordinator.buildTools` (`coordinator.go:652-656`): build both runners:

```go
	var preRunner, postRunner *hooks.Runner
	if hs := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(hs) > 0 {
		preRunner = hooks.NewRunner(hs, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	if hs := c.cfg.Config().Hooks[hooks.EventPostToolUse]; len(hs) > 0 {
		postRunner = hooks.NewRunner(hs, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
```

   and update the wrap call (`coordinator.go:730`) to
   `wrapToolsWithHooks(filteredTools, preRunner, postRunner, isSubAgent)`.

## 9. Stop (`agent.go` + `coordinator.go`)

1. Add `StopHooks *hooks.Runner` to `SessionAgentOptions` and thread it to a
   `stopHooks` field on `sessionAgent` (copy how `DisableAutoSummarize` is
   threaded through `NewSessionAgent`).
2. In `buildAgent` (`coordinator.go:588-601`), when `!isSubAgent`:

```go
	var stopRunner *hooks.Runner
	if hs := c.cfg.Config().Hooks[hooks.EventStop]; len(hs) > 0 {
		stopRunner = hooks.NewRunner(hs, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}
	// pass StopHooks: stopRunner in SessionAgentOptions (nil for subagents)
```

   (Built once at agent construction, unlike the per-run prompt runner:
   acceptable staleness, matches how the PreToolUse runner already behaves
   inside buildTools.)
3. In `publishRunComplete` (grep `func (a *sessionAgent) publishRunComplete`),
   at the top:

```go
	if a.stopHooks != nil {
		outcome := "complete"
		switch {
		case complete.Cancelled:
			outcome = "cancelled"
		case complete.Error != "":
			outcome = "error"
		}
		if _, err := a.stopHooks.RunEvent(ctx, hooks.EventInput{
			Event:     hooks.EventStop,
			SessionID: call.SessionID,
			Outcome:   outcome,
			Error:     complete.Error,
		}); err != nil {
			slog.Warn("Stop hook error, ignoring", "error", err)
		}
	}
```

   Match the actual parameter names of `publishRunComplete` when writing
   this (read the function first). If ctx may already be cancelled on the
   cancel paths, wrap with `context.WithoutCancel(ctx)` — copy the flush
   pattern at `agent.go:769`. Stop hooks run synchronously before the
   terminal event publishes; the 30s default timeout applies — document
   "keep Stop hooks fast" in README.

## 10. Docs & skill updates

1. `docs/hooks/README.md`: add per-event Reference subsections for the three
   new events (payload fields, honored output fields, "matcher: leave empty
   for non-tool events"). State explicitly: UserPromptSubmit `updated_prompt`
   is full-replacement last-writer-wins; the stored/displayed prompt is the
   rewritten one; PostToolUse cannot block execution; Stop is observational.
2. `docs/hooks/FUTURE.md`: delete the `UserPromptSubmit` section (shipped);
   leave `context_files` and sub-agent opt-in.
3. `internal/skills/builtin/crush-hooks/SKILL.md`: update the supported
   events list (line 19), add per-event payload/output tables mirroring
   README.

## 11. Testing

- `internal/hooks/hooks_test.go`:
  - `TestAggregation`: add cases for `UpdatedPrompt` last-writer-wins and
    "UpdatedPrompt empty when unset".
  - `TestParseStdout`: `updated_prompt` string parsed; non-string ignored.
  - `TestBuildEventPayload` (new, imitate `TestBuildPayload`): prompt event
    omits `tool_name`/`tool_input`; post event includes `tool_response`;
    stop event includes `outcome`/`error`; PreToolUse via legacy
    `BuildPayload` is byte-identical to before (golden assert on the JSON).
  - `TestRunnerExitCode*` pattern with fake `runShell` for one new-event
    path (e.g. Stop) proving env `CRUSH_EVENT=Stop`.
- `internal/agent/hooked_tool_test.go` (read it first, reuse `fakeTool` and
  its runner-construction helpers): post-context appended; post-halt sets
  `StopTurn`; post-deny appends `[post-hook]` reason without `IsError`;
  pre-deny → post does not fire; post-only config wraps and pre block
  skipped; `wrapToolsWithHooks(nil, nil, ...)` passthrough.
- UserPromptSubmit: no coordinator test harness change — the logic is a
  self-contained block; if extracting helps, pull it into
  `func (c *coordinator) applyPromptHooks(ctx, sessionID, prompt string) (string, error)`
  and unit-test that with a real `Runner` running `echo` hooks (same trick
  hooked_tool tests use). Do not build a mocking framework.
- Manual script: config with all four events (`jq`-based echo hooks),
  verify: prompt rewrite visible in the message; deny shows the reason and
  no message row is created; post-hook context appears after tool output;
  Stop hook fires once per turn including on Esc-cancel.
- Run `gofumpt -w` on touched files.

## 12. Constraints & non-goals

- The contract: **hook failures never fail a turn** — any code path where a
  hook error propagates as a run/tool error is a bug (except the deliberate
  UserPromptSubmit deny).
- PreToolUse payload/envelope is frozen; any diff to its emitted JSON is a
  bug (golden test in §11 enforces).
- No new dependencies. No async hooks. No sub-agent hook firing.
- Non-goals: `SessionStart`/`SessionEnd` (crush session lifecycle semantics
  are ambiguous — deferred deliberately), `context_files`, sub-agent
  opt-in, attachments in the UserPromptSubmit payload, Claude Code
  `hookSpecificOutput` support for the new events, blocking-Stop
  ("force continue") semantics, storing original+rewritten prompt.

## 13. Milestones

- M1 — hooks package: constants, `EventInput`/`RunEvent`,
  `BuildEventPayload`, `updated_prompt` parsing/aggregation, tests.
  `feat(hooks): event-generic runner input and updated_prompt support`
- M2 — PostToolUse: hookedTool pre/post split, coordinator runner build,
  tests. `feat(hooks): PostToolUse event`
- M3 — UserPromptSubmit: coordinator call site, config normalization for
  all new events, tests. `feat(hooks): UserPromptSubmit event`
- M4 — Stop + docs/skill updates.
  `feat(hooks): Stop event and multi-event docs`

Each milestone compiles and passes tests independently (M3 includes the
`normalizeHookEvent` entries for all three so config keys work as soon as
each event exists).

## 14. Pitfalls checklist (review against this)

1. ❑ PreToolUse emitted payload JSON is byte-identical to before (golden
   test exists and passes).
2. ❑ `normalizeHookEvent` has entries for all three new events, and
   `internal/config` does NOT import `internal/hooks` (grep imports).
3. ❑ No new event fires for subagents: `wrapToolsWithHooks` still bails on
   `isSubAgent`; `StopHooks` nil when `isSubAgent`; UserPromptSubmit lives
   only in `coordinator.run`.
4. ❑ PostToolUse skipped when pre-hook denied and when `inner.Run` returns
   a Go error (grep the early returns).
5. ❑ PostToolUse `tool_input` is the post-rewrite `call.Input`.
6. ❑ `updated_prompt` is last-writer-wins (no shallowMerge call in its
   path) and only consumed by the UserPromptSubmit call site.
7. ❑ Denied UserPromptSubmit creates no user message row (deny happens
   before `currentAgent.Run`; verify no `messages.Create`/
   `createUserMessage` in the new path).
8. ❑ Stop fires exactly once per turn — inside `publishRunComplete` only,
   no additional fire at the defer site or cancel paths.
9. ❑ Stop hook context uses a non-cancelled context on cancel paths
   (`context.WithoutCancel` pattern).
10. ❑ Hook metadata for post hooks lands under `"hook_post"` and the pre
    path still writes `"hook"` (UI untouched).
11. ❑ `hookedTool` with only post hooks still wraps; with neither, tools
    pass through unwrapped.
12. ❑ README/FUTURE/SKILL all updated consistently (FUTURE's
    UserPromptSubmit section removed).
13. ❑ Line numbers in §2 re-located with grep, not trusted blindly.
