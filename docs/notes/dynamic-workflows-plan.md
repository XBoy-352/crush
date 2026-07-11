# Plan: dynamic workflows — a `workflow` tool that runs model-authored JS orchestration scripts

Status: ready to implement. Self-contained: everything needed is in this
document plus the referenced files. Read §2 before writing any code.

This is the hardest feature in the parity list: it introduces an embedded
script runtime, a pattern the codebase does not have. Every novel part is
specced with exact code below. Follow it literally; where a goja API detail
is marked **[verify on install]**, check it against the pinned goja version
before use (instructions in §2.7).

## 1. Goal

A new `workflow` tool for the coder agent. The model writes a small
JavaScript program that orchestrates a fleet of read-only subagents with
deterministic control flow — fan-out over N items, multi-round
loop-until-dry discovery, verification votes — instead of issuing `agent`
tool calls one at a time. The host executes the script in a sandboxed JS
interpreter (goja) exposing exactly three functions:

- `agent(prompt, opts?)` — run one read-only subagent, blocking; returns its
  final text (or parsed JSON with `{json: true}`).
- `parallel(calls)` — run an array of `{prompt, label?, json?}` **data
  objects** (not closures) concurrently; returns
  `[{ok, value, error, label}, ...]` in input order (allSettled semantics —
  it never throws for an individual agent failure).
- `log(message)` — record a progress line, included in the tool result.

The script's `return` value (any JSON-serializable value) becomes the tool
result the coder reads. The whole call is gated by one permission prompt
that shows the script. Spawned agents are the existing read-only **task**
agent; each appears in the TUI nested under the workflow tool call, like
`agent` tool calls do today.

Example script the coder might write (goes in the tool description, §8):

```js
const pkgs = agent("List every Go package under internal/, one per line.").split("\n").filter(Boolean);
const reviews = parallel(pkgs.map(p => ({label: p, prompt: `Review ${p} for error-handling bugs. Reply with a JSON array of findings: [{"file","line","summary"}]`, json: true})));
log(`${reviews.filter(r => r.ok).length}/${pkgs.length} packages reviewed`);
return reviews.filter(r => r.ok).flatMap(r => r.value);
```

Properties to preserve (review criteria):

- **The goja VM is only ever touched from the goroutine that called
  `workflow.Run`** — the single exception is `vm.Interrupt` from the
  cancellation watchdog. `parallel` worker goroutines produce plain Go
  values only; conversion to JS values happens after `wg.Wait()` on the VM
  goroutine.
- Workflow children are **task agents** (read-only tools: glob, grep, ls,
  sourcegraph, view). No workflow code path can reach edit/write/bash. The
  workflow tool itself is never in the task agent's tool list, so scripts
  cannot nest workflows.
- Every workflow run starts with exactly one permission request (unless
  YOLO/allowlisted), and spawned child sessions are auto-approved the same
  way `agentic_fetch` children are.
- Hard caps enforced in the runner, not in the script: 100 agents per
  workflow, 5 concurrent, 64 KiB script, 200 log lines. Exceeding the agent
  cap throws a catchable JS exception; nothing is silently truncated
  without a log.
- Cancelling the session (esc) interrupts the script promptly — including a
  script stuck in a pure JS loop — and no goroutine leaks (repo tests use
  `go.uber.org/goleak`).
- Zero behavior change when the tool is disabled via
  `options.disabled_tools: ["workflow"]`: not registered, absent from the
  coder's tool list.
- Existing `agent`/`agentic_fetch` behavior is untouched except for the
  cost-update mutex (§5.3), which only serializes an already-racy write.

Naming: user-facing "Workflow". Go symbols: `WorkflowToolName = "workflow"`,
runner package `internal/agent/workflow` (files `workflow.go`,
`workflow_test.go`), coordinator glue `internal/agent/workflow_tool.go`,
description `internal/agent/templates/workflow_tool.md`, UI
`internal/ui/chat/workflow.go`.

## 2. Verified codebase facts

Verified against the working tree at commit `28a682af` (branch `feat/btw`).
Line numbers WILL drift — treat them as anchors, re-locate each symbol with
grep before editing.

### 2.1 Subagent tool precedents (`internal/agent/`)

- `agent_tool.go:26-68` — `(c *coordinator) agentTool(ctx)` is the shape the
  new `workflowTool` copies: fetch `c.cfg.Config().Agents[config.AgentTask]`
  (error if missing), build the prompt via `taskPrompt(prompt.WithWorkingDir(...))`
  (`prompts.go:28`), `c.buildAgent(ctx, prompt, agentCfg, true)`, then wrap a
  handler with `fantasy.NewParallelAgentTool`. The handler reads
  `tools.GetSessionFromContext(ctx)` / `tools.GetMessageFromContext(ctx)`
  (`tools/tools.go:45,50`) and errors if either is empty.
- `agentic_fetch_tool.go:83-99` — the permission-request pattern to copy:
  `c.permissions.Request(ctx, permission.CreatePermissionRequest{SessionID,
  Path: c.cfg.WorkingDir(), ToolCallID: call.ID, ToolName, Action,
  Description, Params})`; on `err` return `(fantasy.ToolResponse{}, err)`;
  on `!p` return `tools.NewPermissionDeniedResponse(), nil`.
- `agentic_fetch_tool.go:200-202` — precedent for auto-approving a child
  session: `SessionSetup: func(sessionID string) {
  c.permissions.AutoApproveSession(sessionID) }`.
- `coordinator.go:1291-1301` — `subAgentParams{Agent, SessionID,
  AgentMessageID, ToolCallID, Prompt, SessionTitle, SessionSetup}`.
- `coordinator.go:1306-1379` — `runSubAgent`: creates the child session ID
  via `c.sessions.CreateAgentToolSessionID(params.AgentMessageID,
  params.ToolCallID)` then `CreateTaskSession`; runs the agent with
  `NonInteractive: true`; retries once on 401; **best-effort cost
  propagation** via `updateParentSessionCost` (1365-1372); returns
  `fantasy.NewTextResponse(output)` or a TextErrorResponse. The workflow
  spawn function reuses this wholesale — one call per child agent.
- `coordinator.go:1389-1407` — `updateParentSessionCost` is a
  **read-modify-write with no lock** (`parentSession.Cost += ...; Save`).
  The `agent` tool is already parallel, so this race pre-exists; a workflow
  running 5 concurrent children makes it likely. §5.3 adds a coordinator
  mutex.
- `coordinator.go:624-737` — `buildTools`: conditional tools are appended
  first, gated on `slices.Contains(agent.AllowedTools, <name>)` (see the
  `agent` block at 626-632 and `agentic_fetch` at 634-640 — the workflow
  block copies these). Filtering against `agent.AllowedTools` happens again
  at 695-700, sorting at 725, hook wrapping at 734 (`wrapToolsWithHooks`,
  `hooked_tool.go:31`) — the workflow tool gets PreToolUse hooks for free;
  its children run without hooks, same as all subagents
  (comment at 729-733).
- `coordinator.go:1142-1161` — `UpdateModels` rebuilds all tools **on every
  run**; `agentTool`/`buildAgent` being called per-run is existing,
  accepted behavior, so `workflowTool` doing the same is fine.
- `coordinator.go:581-622` — `buildAgent` populates system prompt and tools
  asynchronously via `c.readyWg`.

### 2.2 Child sessions and the `$$` scheme (`internal/session/session.go`)

- `CreateAgentToolSessionID` (350-353): `messageID + "$$" + toolCallID`.
- `ParseAgentToolSessionID` (355-362): splits on `"$$"`, requires exactly 2
  parts. `IsAgentToolSession` (364-368).
- `CreateTaskSession` (110-122): the first parameter **is used verbatim as
  the session row ID** and the parent ID is stored. Consequence: spawning N
  children from ONE workflow tool call requires N distinct synthetic tool
  call IDs, or the second insert violates the sqlite primary key. The
  synthetic format is `fmt.Sprintf("%s-a%d", call.ID, index)` (§4).

### 2.3 fantasy SDK tool API (`~/go/pkg/mod/charm.land/fantasy@v0.36.0/tool.go`)

- `AgentTool` interface (93-98): `Info() ToolInfo`, `Run(ctx, ToolCall)
  (ToolResponse, error)`, provider-options accessors.
- `NewAgentTool[TInput]` (102-117) — sequential; `NewParallelAgentTool`
  (119-132) — sets `ToolInfo.Parallel = true`. The workflow tool uses
  **`NewAgentTool`** (sequential) — decision 6.
- Params structs use `json` + `description` tags; schema is reflected
  automatically. `ToolResponse{Content, Metadata, IsError}` (32-42);
  helpers `NewTextResponse`, `NewTextErrorResponse`,
  `WithResponseMetadata` (44-89).

### 2.4 Config and registration (`internal/config/config.go`)

- `AgentCoder`/`AgentTask` constants (61-62); `Agent` struct (528-549);
  `Config.Agents` is `json:"-"` (643) — agents are built in code.
- `SetupAgents` (803-828): coder gets
  `resolveAllowedTools(allToolNames(), c.Options.DisabledTools)`; task gets
  `resolveReadOnlyTools(...)` (785-789) = intersection with
  `{glob, grep, ls, sourcegraph, view}`. Adding `"workflow"` to
  `allToolNames()` (748-775) therefore gives it to the coder only; the task
  agent list is unaffected. **Do not touch `resolveReadOnlyTools`.**
- Users disable it with `options.disabled_tools: ["workflow"]` — no new
  config option needed (decision 7).
- `internal/config/load_test.go`: line 691 asserts coder tools ==
  `allToolNames()` (auto-adjusts); lines 713 and 736 are **hardcoded
  expected lists that WILL break** — read the test cases around them to see
  which tools each case disables, and insert `"workflow"` accordingly.
  Lines 695/717/740 (task agent) must stay unchanged.

### 2.5 TUI rendering (`internal/ui/chat/`, `internal/ui/model/`)

- `chat/tools.go` has **four** switch arms keyed on `agent.AgentToolName`
  (lines 248, 1268, 1321, 1639: item construction, params-for-copy,
  params-section, `prettifyToolName`). Grep `agent.AgentToolName` in
  `internal/ui/chat/tools.go` and mirror every arm for
  `agent.WorkflowToolName`.
- `chat/agent.go:20-25` — `NestedToolContainer` interface;
  `AgentToolMessageItem` (27-193) is the exact shape
  `WorkflowToolMessageItem` copies: spinner-until-result (49-51),
  `Animate` override with the parent-bump rationale (65-83),
  `SetNestedTools`/`AddNestedTool` (104-119), `RenderTool` builds a
  `tree.Root(header)` with nested children (167-177).
- `model/ui.go:1502-1560` — `handleChildSessionMessage`: parses the child
  session ID, looks up the chat item **by tool call ID**
  (`m.chat.MessageItem(toolCallID)` at 1521) and appends nested tool items.
  With synthetic IDs (`realID-a3`) the lookup fails and the function
  returns nil — graceful no-op, nothing crashes. §7 adds a suffix-stripping
  fallback so children fold into the workflow item.

### 2.6 What does NOT exist (do not invent)

- No script/JS runtime anywhere; `grep -c goja go.sum` = 0 — goja is a new
  dependency (the only one; do not add `goja_nodejs`).
- No streaming progress channel for tools — a tool reports only its final
  `ToolResponse`. Live per-child progress comes solely from the nested
  session rendering above. Do not build a progress bus.
- No journal/resume, no token-budget accounting hooks, no worktree
  isolation for subagents. All non-goals (§9).
- No per-subagent model override; children use the task agent's models.

### 2.7 New dependency: goja **[verify on install]**

Pin with `go get github.com/dop251/goja@latest` and record the resolved
pseudo-version in the commit message. Pure Go, no cgo; brings
`dlclark/regexp2` and `go-sourcemap/sourcemap`. Before writing the runner,
read the goja README sections on field mapping, exceptions, and
interrupting, and confirm each of these facts against the pinned version
(they are stated from documentation, not from a vendored copy):

1. `goja.New()`, `goja.Compile(name, src, strict)`, `vm.RunProgram`.
2. `vm.Set(name, goGoFunc)` wraps ordinary Go funcs; a non-nil `error` in
   the function's last return position is thrown as a catchable JS
   exception. If the pinned version does not do this for your signature,
   fall back to `func(call goja.FunctionCall) goja.Value` +
   `panic(vm.ToValue(msg))`.
3. `vm.Interrupt(v)` is safe from another goroutine; the running script
   aborts and `RunProgram` returns an `*goja.InterruptedError`.
4. `value.Export()`, `goja.IsUndefined`, `goja.IsNull`,
   `goja.AssertFunction`.
5. A JS→Go argument declared as `map[string]any` / `[]map[string]any`
   receives exported copies of JS objects. If `[]map[string]any` does not
   convert directly, accept `[]any` and assert each element.
6. An async script returns a `*goja.Promise` from `Export()` — detect and
   reject (§4 runner, step 8).

## 3. Architecture decision

```
coder LLM ── workflow(description, script) tool call
                 │  permission prompt (shows script)          [coordinator]
                 ▼
        internal/agent/workflow.Run(ctx, script, spawn, opts)
                 │ goja VM, single goroutine ──────────── watchdog: ctx.Done → vm.Interrupt
                 │   agent(p)      → spawn(ctx, i, label, p)      (blocking)
                 │   parallel([…]) → N goroutines ── semaphore(5) ─┐
                 │        (goroutines return Go values only)       │
                 │   wg.Wait() → JSON round-trip → JS array  ◄─────┘
                 ▼
        spawn = runSubAgent(coordinator.go:1306) per child:
            session ID "msgID$$callID-a<i>", task agent (read-only),
            AutoApproveSession, cost → parent (now under costMu)
                 │
        TUI: child messages → handleChildSessionMessage → strip "-a<i>"
             → WorkflowToolMessageItem.AddNestedTool (flattened tree)
```

Decisions (do not relitigate):

1. **goja, synchronous-only. No event loop, no Promises, no async/await in
   scripts.** Losing option: `goja_nodejs` event loop with promise-returning
   `agent()` (Claude Code semantics). Lost because promise resolution
   requires re-entering the VM from worker goroutines via a loop thread —
   an order of magnitude more integration surface and the top executor risk.
   Plain synchronous JS with a data-parallel `parallel()` covers fan-out,
   loop-until-dry, and vote-verify, which is ~90% of the value.
   Language: JS, not Lua (`gopher-lua`) or Starlark (`starlark-go`) —
   all three embed equally well in Go (pure Go, sandboxed, interruptible),
   but the script author is the LLM, so the deciding factors are model
   fluency (JS is the best-represented language in training data; a
   generation error wastes a whole approved workflow turn), native JSON
   (the entire API surface is JSON-shaped; Lua tables make empty-array vs
   object ambiguous and need a JSON lib), and idiomatic map/filter/flatMap
   for the between-rounds data plumbing. Starlark additionally bans
   `while`, which fights loop-until-dry. Lua's classic embedding
   advantages (size, speed, coroutines) are irrelevant: wall-clock is LLM
   inference and the design is synchronous-only.
2. **`parallel()` takes an array of data objects, never closures.** A JS
   callback cannot run on a worker goroutine (single-VM-goroutine rule).
   Consequence: no `pipeline()`; multi-stage work is expressed as
   successive `parallel()` rounds with plain JS transforms between them
   (barrier semantics). Documented in the tool description.
3. **Children are the existing task agent** (read-only toolset, no MCPs) via
   `taskPrompt` + `buildAgent(..., true)` — byte-identical to the `agent`
   tool's children. Losing option: coder-toolset children for parallel
   mutation — lost because concurrent writes without worktree isolation are
   unsafe; mutation stays in the main agent.
4. **Reuse `runSubAgent` and the `$$` session scheme with synthetic tool
   call IDs `<call.ID>-a<index>`.** Losing option: a new session type or a
   single shared child session — lost because per-child sessions give
   cost accounting, TUI rendering, and DB persistence for free.
5. **One permission request per workflow call** (action `execute`,
   description = model-supplied summary, params carry the script so the
   dialog can show it). Children are auto-approved
   (`AutoApproveSession`) — precedent `agentic_fetch_tool.go:200-202`; their
   toolset is read-only. Users can allowlist `workflow` in
   `permissions.allowed_tools` like any tool.
6. **The workflow tool is sequential (`NewAgentTool`), not parallel.** Two
   concurrent workflows would multiply agent fleets invisibly; one at a
   time keeps cost and the TUI legible.
7. **No new config option.** Disable via existing
   `options.disabled_tools: ["workflow"]`. Caps are constants, not knobs.
8. **`json: true` = fence-strip + first-JSON-value extraction, throwing a
   catchable JS exception on failure.** Losing option: schema validation
   (`kaptinlin/jsonschema` is already an indirect dep) — lost because
   forcing subagent output through a schema needs a retry protocol we don't
   have; extraction plus script-side checks is enough for v1.
9. **Cost race fixed with a coordinator mutex** around
   `updateParentSessionCost`, benefiting the existing parallel `agent` tool
   too. Losing option: defer-and-sum-once in the workflow — lost because it
   forks the accounting path; a mutex is 3 lines.

## 4. API / type spec

### Runner package — `internal/agent/workflow/workflow.go`

```go
// Package workflow executes model-authored JavaScript orchestration
// scripts in a sandboxed goja interpreter. The VM runs on the calling
// goroutine only; concurrency happens exclusively inside parallel(),
// whose workers produce plain Go values.
package workflow

const (
	DefaultMaxConcurrent = 5
	DefaultMaxAgents     = 100
	MaxScriptBytes       = 64 * 1024
	maxLogEntries        = 200
	maxLogEntryBytes     = 2048
)

// SpawnFunc runs one subagent. index is the global 0-based agent index
// (unique per workflow run, used for synthetic tool-call IDs), label an
// optional display title, prompt the task. Returns the agent's final text.
type SpawnFunc func(ctx context.Context, index int, label, prompt string) (string, error)

type Options struct {
	MaxConcurrent int // <=0 → DefaultMaxConcurrent
	MaxAgents     int // <=0 → DefaultMaxAgents
}

type Result struct {
	Value      string   // JSON-encoded script return value; "" if undefined/null
	Logs       []string // log() lines, capped
	AgentCount int      // agents actually started
}

// Run compiles and executes script. It returns ctx.Err() if canceled.
// Script exceptions and syntax errors come back as ordinary errors with
// the JS message preserved.
func Run(ctx context.Context, script string, spawn SpawnFunc, opts Options) (Result, error)
```

JS surface (exact semantics the runner must implement):

| JS call | behavior |
|---|---|
| `agent(prompt)` | blocks, returns string. Throws on: empty/non-string prompt, agent cap exceeded, spawn error, cancellation. |
| `agent(prompt, {json: true})` | as above; result passed through `extractJSON`; throws (catchable) if no JSON value found. |
| `parallel(calls)` | `calls`: non-empty array of `{prompt: string, label?: string, json?: bool}`. Validates ALL entries and the cap **before** starting any. Returns array (input order) of `{ok: true, value, label}` or `{ok: false, error: string, label}`. Throws only on validation/cap failure or cancellation. |
| `log(msg)` | coerces to string, truncates to 2048 bytes, keeps first 200 entries (append `"(further logs dropped)"` once). |
| `return <value>` | value must be JSON-serializable; becomes `Result.Value`. |

### Coordinator glue — `internal/agent/workflow_tool.go`

```go
//go:embed templates/workflow_tool.md
var workflowToolDescription string

const WorkflowToolName = "workflow"

type WorkflowParams struct {
	Description string `json:"description" description:"One-line summary of what this workflow does; shown in the permission prompt"`
	Script      string `json:"script" description:"JavaScript orchestration script; see tool description for the API"`
}

func (c *coordinator) workflowTool(ctx context.Context) (fantasy.AgentTool, error)
```

Tool response content on success (exact format):

```
Workflow finished: <N> agent(s) run.

Return value:
```json
<Result.Value or "null">
```

Logs:
<one "- " bullet per log line, or "(none)">
```

On script failure: `NewTextErrorResponse("workflow failed: <err>\n\nLogs:\n<bullets>")`.
Attach `WithResponseMetadata(resp, map[string]any{"agents": n, "logs": logs})`.

## 5. Runner implementation (`internal/agent/workflow/workflow.go`) — build first

Self-contained, no crush imports beyond stdlib + goja. Steps:

1. `Run` validates `len(script) <= MaxScriptBytes`, applies `Options`
   defaults, creates `vm := goja.New()` and a `runState` holding: `ctx`,
   `spawn`, the options, `logs []string`, an `agentIndex` counter, and
   `sem := make(chan struct{}, opts.MaxConcurrent)`. Counter and logs are
   mutex-guarded (parallel workers touch neither — only the coordinator
   goroutine does — but the mutex makes that a non-issue for reviewers).
2. Watchdog (exact code):

   ```go
   watchdogDone := make(chan struct{})
   defer close(watchdogDone)
   go func() {
   	select {
   	case <-ctx.Done():
   		vm.Interrupt(ctx.Err())
   	case <-watchdogDone:
   	}
   }()
   ```

   The `defer close` guarantees the goroutine exits when `Run` returns
   (goleak — §2.7 fact 3 for Interrupt safety).
3. Register hosts: `vm.Set("agent", st.jsAgent)`, `vm.Set("parallel",
   st.jsParallel)`, `vm.Set("log", st.jsLog)`. Signatures per §2.7 fact 2;
   on doubt, use the `goja.FunctionCall` fallback form for all three.
4. Compile `"(function(){\n" + script + "\n})()"` as file name
   `"workflow.js"` (so JS stack traces line up minus one), strict mode on.
   Compile error → `fmt.Errorf("script error: %w", err)`.
5. `v, err := vm.RunProgram(prog)`. If `err != nil`: when `ctx.Err() != nil`
   return `Result{...}, ctx.Err()` (interrupt case); otherwise
   `fmt.Errorf("workflow script failed: %w", err)` — goja's error string
   contains the JS exception message and stack.
6. `jsAgent(prompt string, opts map[string]any)`:
   reserve an index (`st.nextIndex()` — increments and errors with
   `"workflow agent limit (100) reached"` once past `MaxAgents`); acquire
   `st.sem` (select against `ctx.Done()`); call
   `spawn(ctx, idx, optString(opts, "label"), prompt)`; release; if
   `opts["json"] == true` return `extractJSON(text)` converted via
   `toJSValue`, else return the string.
7. `jsParallel(calls []map[string]any)`: validate every entry first
   (non-empty string `prompt`; anything else → error naming the index);
   reserve ALL indices up front (cap check is atomic: `n + len(calls) >
   MaxAgents` → error before any spawn); then:

   ```go
   results := make([]spawnResult, len(calls))
   var wg sync.WaitGroup
   for i, call := range calls {
   	wg.Add(1)
   	go func(i int, call parsedCall) {
   		defer wg.Done()
   		select {
   		case st.sem <- struct{}{}:
   			defer func() { <-st.sem }()
   		case <-st.ctx.Done():
   			results[i] = spawnResult{err: st.ctx.Err()}
   			return
   		}
   		text, err := st.spawn(st.ctx, call.index, call.label, call.prompt)
   		results[i] = spawnResult{text: text, err: err}
   	}(i, parsed[i])
   }
   wg.Wait()
   // Back on the VM goroutine: build []any of result maps (apply
   // extractJSON where call.json), then convert ONCE via toJSValue.
   ```

   **No goja call of any kind inside the goroutine** — this is checklist
   item 1.
8. `toJSValue(vm, goValue)`: `json.Marshal` the Go value, then invoke the
   VM's own `JSON.parse` via `goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))`.
   This sidesteps all Go↔JS wrapping subtleties: scripts always see native
   JS arrays/objects. Result handling in `Run`: if `v` is
   undefined/null → `Value = ""`; if `v.Export()` is a `*goja.Promise` →
   error `"script returned a Promise; async/await is not supported"`
   (§2.7 fact 6); else `json.Marshal(v.Export())` → `Value` (marshal error
   → error naming the offending type).
9. `extractJSON(s string)`: trim space; strip a leading ```` ```json ````
   or ```` ``` ```` fence and trailing fence; try `json.Unmarshal` of the
   whole; else take the substring from the first `{` or `[` to the last
   `}` or `]` and try again; else `error("no JSON value found in agent
   reply")`. Pure function — main unit-test surface alongside the caps.

## 6. Coordinator glue (`internal/agent/workflow_tool.go`)

Copy `agent_tool.go`'s constructor shape exactly, then the handler:

1. Constructor: task agent config + `taskPrompt` + `buildAgent(ctx, prompt,
   agentCfg, true)` — identical lines to `agent_tool.go:27-39`. Return via
   `fantasy.NewAgentTool` (decision 6).
2. Handler steps, in order:
   - Validate: `Script` non-empty, `Description` non-empty,
     `len(Script) <= workflow.MaxScriptBytes` — each failure a
     `NewTextErrorResponse` naming the rule.
   - Session/message from context — copy `agent_tool.go:48-56`.
   - Permission request — copy `agentic_fetch_tool.go:83-99` with
     `ToolName: WorkflowToolName`, `Action: "execute"`,
     `Description: params.Description`, `Params: params` (the dialog then
     shows the script like it shows a bash command).
   - `spawn` closure (exact):

     ```go
     spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
     	title := label
     	if title == "" {
     		title = fmt.Sprintf("Workflow Agent %d", index+1)
     	}
     	resp, err := c.runSubAgent(ctx, subAgentParams{
     		Agent:          agent,
     		SessionID:      sessionID,
     		AgentMessageID: agentMessageID,
     		ToolCallID:     fmt.Sprintf("%s-a%d", call.ID, index),
     		Prompt:         prompt,
     		SessionTitle:   title,
     		SessionSetup:   func(id string) { c.permissions.AutoApproveSession(id) },
     	})
     	if err != nil {
     		return "", err
     	}
     	if resp.IsError {
     		return "", errors.New(resp.Content)
     	}
     	return resp.Content, nil
     }
     ```

   - `result, err := workflow.Run(ctx, params.Script, spawn, workflow.Options{})`,
     then format per §4. A `ctx.Err()` result returns
     `(fantasy.ToolResponse{}, err)` so the turn cancels normally.

### 6.1 Registration

1. `buildTools` (`coordinator.go:624`): after the `agentic_fetch` block
   (634-640), add the same three-line gated append for
   `WorkflowToolName` / `c.workflowTool(ctx)`.
2. `allToolNames()` (`config.go:748`): append `"workflow"` at the end of
   the slice.
3. `updateParentSessionCost` mutex: add `costMu sync.Mutex` to the
   `coordinator` struct (`coordinator.go:108`) and
   `c.costMu.Lock(); defer c.costMu.Unlock()` as the first lines of
   `updateParentSessionCost` (`coordinator.go:1389`).
4. `go get github.com/dop251/goja@latest && go mod tidy`.
5. Update `internal/config/load_test.go` per §2.4. Run
   `go test ./internal/config/`.

## 7. TUI (`internal/ui/chat/workflow.go`, `internal/ui/model/ui.go`)

1. New `chat/workflow.go`: `WorkflowToolMessageItem` — copy
   `AgentToolMessageItem` (`agent.go:27-193`) including the `Animate`
   override and its comment rationale, renaming, with `RenderTool`
   differences only: pending label `"Workflow"`, params from
   `agent.WorkflowParams`, tag text `"Workflow"` rendered with the existing
   `sty.Tool.AgentTaskTag` style (do NOT add a new style field), body =
   `params.Description` on one line.
2. Mirror all four `agent.AgentToolName` switch arms in `chat/tools.go`
   (§2.5): construct `NewWorkflowToolMessageItem`; params-for-copy shows
   `**Description:** …` plus the script in a `js` fence; result-for-copy
   same as agent's markdown fence; `prettifyToolName` → `"Workflow"`.
3. `model/ui.go handleChildSessionMessage` (1502): after the existing
   lookup loop leaves `agentItem == nil`, retry once with the suffix
   stripped:

   ```go
   if agentItem == nil {
   	if base, ok := workflowBaseCallID(toolCallID); ok {
   		// … re-run the same lookup loop with base …
   	}
   }
   ```

   with `var workflowCallIDRe = regexp.MustCompile(`^(.+)-a\d+$`)` and
   `workflowBaseCallID` returning the first capture group. Extract the
   existing lookup body into a small helper so the loop isn't duplicated.
   Children of all workflow agents then fold, flattened and compact, under
   the workflow item — accepted v1 rendering.

## 8. Tool description (`internal/agent/templates/workflow_tool.md`)

Write it with these sections (model-facing; keep under ~120 lines): what a
workflow is; **when to use** — only when the user explicitly asks for a
workflow / parallel or comprehensive multi-agent work, or a task genuinely
needs >3 similar independent subagent runs; never for single lookups; **the
agents are read-only** (research, review, audit — never edits; do the
editing yourself afterwards); the full JS API table from §4 including the
allSettled result shape; **plain synchronous JavaScript only — no
async/await/Promises/require/filesystem/network**; state that `parallel`
takes data objects, with the §1 example plus a loop-until-dry example; the
caps (100 agents, 5 concurrent); "prefer one `parallel` round per stage;
put transforms between rounds in plain JS"; "always `return` a
JSON-serializable summary".

Do NOT edit `coder.md.tpl` — guidance lives entirely in the tool
description so the system prompt (and `TestCoderAgent` cassettes) keep
their static text. Note: adding any tool still changes the outbound
request's tool list; run `go test ./internal/agent/` for a green baseline
first, and if cassette matching fails, follow the re-record procedure in
`internal/agent/common_test.go` (same fallback as the memory plan) — do not
weaken the matcher.

## 9. Testing

- `internal/agent/workflow/workflow_test.go` — table-driven, fake
  `SpawnFunc`s, no VCR, no network. Required cases:
  - return value: object/array/string/number marshaled; no `return` → `Value == ""`.
  - `agent()` passes prompt/label through; empty prompt throws a catchable
    JS exception (`try/catch` in the test script proves catchability).
  - `parallel`: results in input order; one failing spawn yields
    `{ok:false,error}` without failing siblings; invalid entry (missing
    prompt) throws before any spawn (assert fake spawn never called).
  - concurrency: barrier fake — first `MaxConcurrent` spawns block on a
    channel until the test confirms none beyond the cap started (use a
    counter + short sleep, generous timeouts; see `internal/shell`
    background tests for the repo's concurrency-test idiom, and avoid
    time-based flakiness).
  - caps: 101st agent throws; script catching it and returning still
    succeeds; log caps applied.
  - cancellation: script `for(;;){}` plus a 100ms-cancel context → `Run`
    returns `context.Canceled` promptly (assert < 2s); goleak-clean
    (add `goleak.VerifyTestMain` in this new package's `TestMain`).
  - `extractJSON`: fenced, bare, prose-wrapped, garbage → error.
  - syntax error → error containing the JS message; async script (`return
    (async () => 1)()`) → the Promise rejection error from §5 step 8.
- `internal/config/load_test.go` updates (§2.4).
- No new UI tests (no precedent for message-item unit tests; manual only).
- Manual script: build; in this repo ask crush *"use a workflow to list
  every package under internal/ and count its TODO comments in parallel,
  then give me a table"* — verify: one permission prompt showing the
  script; nested children appear under the Workflow item; esc mid-run
  cancels within ~1s; final table renders; `crush stats` shows child costs
  rolled up.
- `gofumpt -w` on all touched files.

## 10. Constraints & non-goals

- The contract: **goja is touched from one goroutine** (plus `Interrupt`),
  **children are task agents only**, and **the runner package imports no
  crush packages** — a violation of any of these is a bug, not a judgment
  call.
- Only new dependency: `github.com/dop251/goja` (+ its transitive deps).
  No `goja_nodejs`, no JS polyfills, no jsonschema validation.
- Non-goals (do not build any of this): `pipeline()`/`phase()`, promises or
  an event loop, journal/resume, token budgets, nested workflows, saved or
  named workflows, workflow args, worktree isolation, mutating children,
  per-agent model/effort overrides, a phase-tree TUI, streaming progress
  events, config knobs for the caps.

## 11. Milestones

- M1 — runner package + goja dep + full test suite (nothing registered;
  no crush code touched). `feat(workflow): add sandboxed JS workflow runner`
- M2 — coordinator tool, cost mutex, registration, config test updates,
  tool description. `feat(agent): add workflow tool for multi-agent orchestration`
- M3 — TUI item, four switch arms, child-routing fallback, manual test
  pass. `feat(ui): render workflow tool with nested agent sessions`

Each milestone compiles and passes `go test ./...` independently.

## 12. Pitfalls checklist (review against this)

1. ❑ No goja identifier appears inside a goroutine closure in the runner
   except `vm.Interrupt` in the watchdog (grep `go func` blocks in
   `workflow.go`).
2. ❑ `parallel` results and `json:true` values reach JS via the
   `toJSValue` JSON round-trip — no `vm.ToValue` of Go maps/slices
   anywhere.
3. ❑ Synthetic tool call IDs use exactly `%s-a%d` and index uniqueness
   comes from one shared counter (grep `-a` format string; one call site).
4. ❑ Cap check in `jsParallel` happens before the first spawn (reserve-all
   pattern present).
5. ❑ `resolveReadOnlyTools`, `task.md.tpl`, and `coder.md.tpl` untouched
   (git diff shows no change).
6. ❑ Workflow children get `AutoApproveSession`; no other permission
   changes; the workflow tool itself calls `permissions.Request` exactly
   once.
7. ❑ `updateParentSessionCost` body starts with the mutex lock; no other
   accounting path added.
8. ❑ Watchdog goroutine provably exits (`defer close(watchdogDone)`);
   workflow package `TestMain` uses goleak.
9. ❑ `workflow` in `allToolNames()`; load_test 691/713/736 updated;
   695/717/740 task lists unchanged; `go test ./internal/config/` green.
10. ❑ Tool registered via `NewAgentTool` (sequential), gated on
    `AllowedTools`, wrapped by `wrapToolsWithHooks` like every other tool
    (no special-casing in the hook path).
11. ❑ All four `agent.AgentToolName` switch arms in `ui/chat/tools.go`
    have `WorkflowToolName` counterparts.
12. ❑ Tool description documents only `agent`/`parallel`/`log`/`return`
    and states the read-only + no-async constraints (grep it for
    "pipeline" and "await" — the only hits must be the "not supported"
    statements).
13. ❑ `TestCoderAgent` green (cassettes re-recorded if needed, committed
    with the change).
14. ❑ goja facts from §2.7 verified against the pinned version before the
    runner was written (note the resolved version in the M1 commit).
15. ❑ Line numbers in §2 re-located with grep, not trusted blindly.
