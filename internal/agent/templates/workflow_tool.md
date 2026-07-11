# Workflow

A workflow allows you to write and execute a JavaScript orchestration script to run a fleet of read-only subagents with deterministic control flow.

## When to use

- Only use when the user explicitly asks for a workflow, parallel execution, or comprehensive multi-agent work.
- Use if a task genuinely needs > 3 similar independent subagent runs.
- **NEVER** use for single lookups or simple queries.
- **The agents are read-only**: They can research, review, and audit, but they **cannot edit files**. You must perform any required edits yourself after the workflow completes.

## Script API

You must write **plain synchronous JavaScript only**. There is no async/await, Promises, require, filesystem access, or network access.

| JS call | behavior |
|---|---|
| `agent(prompt)` | blocks, returns string. Throws on: empty/non-string prompt, agent cap exceeded, spawn error, cancellation. |
| `agent(prompt, {json: true})` | as above; result passed through JSON extractor; throws (catchable) if no JSON value found. |
| `parallel(calls)` | `calls`: non-empty array of `{prompt: string, label?: string, json?: bool}` data objects (not closures). Validates ALL entries and the cap **before** starting any. Returns array (input order) of `{ok: true, value, label}` or `{ok: false, error: string, label}`. Throws only on validation/cap failure or cancellation. |
| `log(msg)` | coerces to string, truncates to 2048 bytes, keeps first 200 entries (append `"(further logs dropped)"` once). |
| `return <value>` | value must be JSON-serializable; becomes the workflow return value. |

## Examples

### Fan-out Example
```js
const pkgs = agent("List every Go package under internal/, one per line.").split("\n").filter(Boolean);
const reviews = parallel(pkgs.map(p => ({label: p, prompt: `Review ${p} for error-handling bugs. Reply with a JSON array of findings: [{"file","line","summary"}]`, json: true})));
log(`${reviews.filter(r => r.ok).length}/${pkgs.length} packages reviewed`);
return reviews.filter(r => r.ok).flatMap(r => r.value);
```

### Loop-until-dry Example
```js
let items = [];
for (let i = 0; i < 3; i++) {
  const result = agent("Find the next page of results.", {json: true});
  if (!result || result.length === 0) break;
  items.push(...result);
}
return items;
```

## Constraints and Caps
- **Max Agents**: 100 agents per workflow.
- **Max Concurrent**: 5 agents running concurrently.
- Prefer one `parallel` round per stage. Put data transforms between rounds in plain JS.
- You must always `return` a JSON-serializable summary.
