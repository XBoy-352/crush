# Claude Code parity — missing / partial feature list

Gap analysis of this crush fork vs. Claude Code (as of 2026-07). Crush inventory
taken from this codebase; Claude Code list from official docs. Ordered roughly
by how feasible + valuable each gap is for a terminal agent.

## Already at parity (no work needed)

Read/write/edit/multiedit, bash with background jobs (`job_output`/`job_kill`),
glob/grep/ripgrep, web fetch (incl. agentic fetch subagent) + web search, todos,
subagents (`agent` tool), LSP + diagnostics, MCP tools/prompts/resources over
stdio/http/sse, permission prompts + allowlists + YOLO, sessions + resume +
auto-summarize, custom markdown commands with args, skills (SKILL.md), context
files (CRUSH.md/CLAUDE.md/AGENTS.md/.cursorrules), multi-provider + model
switching + reasoning effort, provider OAuth logins, headless `crush run`,
image attachments, `/revert`, telemetry opt-out, `.env` + shell expansion in
config.

## Tier 1 — core gaps, feasible in a TUI binary

| # | Feature | Status | Notes |
|---|---------|--------|-------|
| 1 | **Plan mode** | Missing | Read-only analysis mode with an approval gate before edits (Claude Code: `plan` permission mode + ExitPlanMode). Crush has only default-prompt and YOLO. |
| 2 | **More hook events** | Partial | Only `PreToolUse` exists (`internal/hooks/`, already Claude Code JSON-compatible). Missing: `PostToolUse`, `UserPromptSubmit`, `SessionStart`/`SessionEnd`, `Stop`, `Notification`, `PreCompact`/`PostCompact`, `SubagentStart`/`Stop`. Roadmap already sketched in `docs/hooks/FUTURE.md`. |
| 3 | **Permission modes between "ask" and YOLO** | Partial | Claude Code has `acceptEdits` (auto-approve file edits only) and `dontAsk` (auto-approve allowlisted tools only). Crush jumps from per-call prompts straight to `--yolo`. |
| 4 | **User-defined custom agents** | Partial | `config.Agent` supports model/tools/MCP filters but `Config.Agents` is `json:"-"` (`internal/config/config.go:642`) — users can't define agents. Claude Code: markdown agent defs in `.claude/agents/` with frontmatter (tools, model, prompt). |
| 5 | **Structured headless output** | Partial | `crush run` streams plain text only. Claude Code: `--output-format json` / `stream-json`, JSON-schema-validated structured output. Needed for scripting/CI use. |
| 6 | **Auto memory** | Missing | Persistent per-project memory the agent writes/reads across sessions (Claude Code: `~/.claude/projects/<hash>/memory/` + `/memory` command). Crush only has static context files. |
| 7 | **Per-turn checkpointing / rewind** | Partial | `/revert` (message picker, restores files + session) covers the core. Missing: automatic checkpoint on every turn, rewind-with-summary mode, double-esc UX. |
| 8 | **MCP OAuth** | Missing | Remote MCP servers only auth via static `Headers`. Claude Code does the full OAuth handshake (callback server, token refresh, metadata discovery). |
| 9 | **AskUserQuestion-style tool** | Missing | Agent-initiated structured multi-choice questions to the user. Crush only has permission dialogs. |
| 10 | **Path-scoped rules** | Missing | Claude Code: `.claude/rules/*.md` and per-directory CLAUDE.md layering for monorepos. Crush loads flat context paths only. |
| 11 | **Session forking/branching** | Missing | Duplicate a session to explore an alternative without losing the original. Crush sessions are linear (revert destroys the tip). |
| 12 | **Bash sandboxing** | Missing | Claude Code: filesystem/network isolation for shell commands. Crush's `mvdan/sh` interpreter has command blocking but no FS/network isolation. |
| 13 | **`/context` + `/usage` visibility** | Partial | Crush tracks tokens and has `crush stats`, but no in-session context-window breakdown or cost/cache view. |
| 14 | **Prompt/agent/HTTP hooks** | Missing | Crush hooks are shell-command only. Claude Code also supports prompt-evaluated, subagent, HTTP, and async hooks. |
| 15 | **Permission rule granularity** | Partial | Crush: `tool` / `tool:action` keys. Claude Code: path globs, bash command-prefix patterns, compound-command analysis, read-only detection, deny rules. |
| 16 | **Dynamic workflows (ultracode)** | Missing | Deterministic multi-agent orchestration: the model authors a JS script at runtime with `agent()` / `parallel()` / `pipeline()` / `phase()` primitives, then the harness executes it — fan-out over N items, loop-until-dry discovery, adversarial verify panels, token-budget-driven scaling, resume-from-cache after edits. "Ultracode" = opt-in mode where every substantive task gets a workflow by default. Distinct from crush's `agent` tool: that is one level of model-driven delegation; workflows are *deterministic control flow over fleets of agents*. Also: saved/named workflows reusable with args. |

## Tier 2 — nice-to-have polish

| # | Feature | Status | Notes |
|---|---------|--------|-------|
| 16 | Output styles | Missing | User-selectable response style presets (markdown files acting as system-prompt overlays). |
| 17 | Status line customization | Missing | User script that renders the status bar (model, git, cost, context %). |
| 18 | Keybindings config file | Unknown/Partial | Claude Code: `keybindings.json` with contexts + chords. Check what crush's TUI exposes. |
| 19 | NotebookEdit | Missing | Cell-level Jupyter editing. Niche for crush's audience. |
| 20 | PDF reading | Missing | Claude Code's Read handles PDFs via vision. |
| 21 | Self-installing auto-update | Partial | `internal/update/` only notifies; no download-and-replace, no release channels. |
| 22 | OpenTelemetry export | Partial | Crush has its own metrics (`internal/event/`); no OTel metrics/traces for org monitoring. |
| 23 | Git worktree UX | Partial | `internal/workspace/` exists; no `-w <branch>` flag / worktree-per-task flow or subagent worktree isolation. |
| 24 | Advisor / second-opinion model | Missing | Consult a second model on hard decisions. Crush's large/small split is the closest analog. |
| 25 | Plugins | Missing | Bundling commands+skills+MCP+hooks into installable packages with a marketplace. Crush skills/commands cover part of this. |

## Tier 3 — platform features (out of scope for a single Go binary)

Listed for completeness; these need hosted infrastructure or separate clients,
not crush code:

- IDE extensions (VS Code, JetBrains) — though `crush server` + swagger API is a foundation an extension could talk to
- Desktop app, web app (claude.ai/code), iOS, teleport between surfaces, remote control
- Cloud routines / scheduled agents, dispatch, Slack integration
- GitHub Actions `@claude` bot, GitHub Code Review app, GitLab CI integration
- Artifacts (hosted shareable pages)
- Chrome extension / computer use
- Enterprise: managed settings, SSO/SAML, gateways, ZDR, audit logging, spend caps
- Agent SDK (crush's client/server split is the seed of this)

## Suggested build order

1. Plan mode + `acceptEdits` mode (#1, #3) — biggest daily-driver gap
2. PostToolUse + UserPromptSubmit + Stop hooks (#2) — engine exists, mostly wiring
3. JSON output for `crush run` (#5) — small, unlocks scripting
4. User-defined agents in config (#4) — remove `json:"-"`, add validation
5. Auto memory (#6)
6. Dynamic workflows / ultracode (#16) — needs a sandboxed script runner + agent-pool
   scheduler on top of the existing subagent machinery; biggest single feature here
7. MCP OAuth (#8)
