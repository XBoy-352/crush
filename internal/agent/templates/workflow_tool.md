# Workflow

A workflow allows you to write and execute a Lua orchestration script to run a fleet of read-only subagents with deterministic control flow.

## When to use

- Only use when the user explicitly asks for a workflow, parallel execution, or comprehensive multi-agent work.
- Use if a task genuinely needs > 3 similar independent subagent runs.
- **NEVER** use for single lookups or simple queries.
- **The agents are read-only**: They can research, review, and audit, but they **cannot edit files**. You must perform any required edits yourself after the workflow completes.

## Script API

You must write **plain synchronous Lua only**. There is no async, require, filesystem access, or network access.

| Lua call | behavior |
|---|---|
| `agent(prompt)` | blocks, returns string. Throws on: empty/non-string prompt, agent cap exceeded, spawn error, cancellation. |
| `agent(prompt, {json = true})` | as above; result passed through JSON extractor; throws (catchable via pcall) if no JSON value found. |
| `parallel(calls)` | `calls`: non-empty array (1-indexed table) of `{prompt = string, label = string, json = bool}` tables. Validates ALL entries and the cap **before** starting any. Returns array (input order) of `{ok = true, value, label}` or `{ok = false, error = string, label}`. Throws only on validation/cap failure or cancellation. |
| `log(msg)` | coerces to string, truncates to 2048 bytes, keeps first 200 entries (appends `"(further logs dropped)"` once). |
| `return <value>` | value must be JSON-serializable; becomes the workflow return value. |

## Examples

### Fan-out Example
```lua
local pkgs = agent("List every Go package under internal/, one per line.")
local list = {}
for p in pkgs:gmatch("[^\n]+") do
  list[#list+1] = p
end
local calls = {}
for i, p in ipairs(list) do
  calls[i] = {
    label = p,
    prompt = "Review " .. p .. " for error-handling bugs. Reply with a JSON array of findings: [{\"file\",\"line\",\"summary\"}]",
    json = true
  }
end
local reviews = parallel(calls)
local ok = 0
for _, r in ipairs(reviews) do
  if r.ok then ok = ok + 1 end
end
log(ok .. "/" .. #list .. " packages reviewed")
local findings = {}
for _, r in ipairs(reviews) do
  if r.ok then
    for _, f in ipairs(r.value) do
      findings[#findings+1] = f
    end
  end
end
return findings
```

### Loop-until-dry Example
```lua
local items = {}
for i = 1, 3 do
  local result = agent("Find the next page of results.", {json = true})
  if not result or #result == 0 then break end
  for _, v in ipairs(result) do
    items[#items+1] = v
  end
end
return items
```

## Constraints and Caps
- **Max Agents**: 100 agents per workflow.
- **Max Concurrent**: 5 agents running concurrently.
- Prefer one `parallel` round per stage. Put data transforms between rounds in plain Lua.
- You must always `return` a JSON-serializable summary.
- Use `pcall` to catch errors from `agent()` and `parallel()`.
