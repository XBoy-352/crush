# Plan template — for specs executed by a cheaper model

This template produces implementation plans that a mid-tier model (grok,
Gemini, GLM) can execute with minimal judgment, and that a top-tier model
can review cheaply afterwards. It is distilled from
`telegram-bridge-plan.md` and `btw-plan.md`, both of which went through
the full plan → build → review cycle.

Keep plan docs OUT of feature PRs. They live locally (or on a scratch
branch); only the feature code ships.

---

## Rules for the plan AUTHOR

1. **Verify every fact by reading source.** Nothing in §2 may come from
   memory or inference. Read the file, record `path:line`, and note the
   commit hash the lines were verified at. The executor re-locates by
   grep; the anchors just prove you looked.
2. **Lock decisions; do not offer options.** "Use X" — never "consider X
   or Y". Every open choice you leave becomes a coin-flip made by a
   weaker model. If a decision was close, record the losing option and
   why it lost, under "do not relitigate".
3. **Point at existing patterns, don't invent.** For each new function,
   name the existing function whose shape it copies ("copy
   `SummarizeSession`'s shape"). The executor imitates far better than
   it designs. If the codebase lacks a pattern, that part of the feature
   is high-risk: spec it in full detail (exact code) or descope it.
4. **Make the checklist reviewable.** Each pitfall item must be checkable
   by grep or by reading one function — "no `messages.Create` anywhere in
   the new path", not "handle persistence correctly".
5. **Spell out the traps you already know.** Anything you know that the
   executor can't derive from the code (debounced writes, unsupported
   flags on this device, provider quirks) goes in §2 as a stated fact.
6. **Write instructions for the executor's process, not just the code.**
   E.g. "trace how X routes before writing the dialog", "grep this
   symbol before editing", "run gofumpt on touched files".

## Rules for the plan VERIFIER (bounded one-shot check)

1. Grep-check a sample (or all) of §2's `path:line` claims: does the
   symbol exist, does the described behavior match the code?
2. For each §3 decision, confirm the stated justification is true in the
   code (e.g. "no per-request streaming precedent exists" — grep it).
3. Check every checklist item is concretely verifiable (rule A4).
4. Check nothing in the API spec contradicts an existing route/type.
5. Report discrepancies only; do not redesign.

---

## Section skeleton

# Plan: <feature> — <one-line description>

Status: ready to implement. Self-contained: everything needed is in this
document plus the referenced files. Read §2 before writing any code.

## 1. Goal

What the feature does, from the user's point of view. Then "Properties
to preserve" — the bulleted invariants that define done (these become
review criteria). Then naming: user-facing name vs Go symbol names.

## 2. Verified codebase facts

Opening line: "Verified against the tree at commit `<hash>` (branch
`<name>`). Line numbers WILL drift — treat them as anchors, re-locate
each symbol with grep before editing."

Grouped by subsystem. Each fact: what exists, where (`path:line`), and
why it matters for this feature. Include:
- the existing functions whose shape each new piece copies
- infrastructure quirks (debounce, locking, concurrency-unsafe helpers)
- conventions (JSON tag style, error-mapping table, test idioms)
- what does NOT exist (so the executor doesn't invent it)

## 3. Architecture decision

An ASCII diagram of the feature threaded through the existing layers.
Then "Decisions (do not relitigate):" — numbered, each with its one-line
justification.

## 4. API / type spec

Exact code: type definitions with tags, route strings with status-code
semantics, function signatures. The executor should be able to paste
these.

## 5..N. Layer-by-layer implementation

One section per layer/component, in build order. Each: signature, then
numbered steps where every step maps to a §2 fact ("step 3 = same as
Summarize, `agent.go:1645`"). Include exact code skeletons for anything
novel, and "grep X before writing this" instructions where an API detail
wasn't verified.

## N+1. Testing

Which tests, in which packages, following which existing pattern (name
the file to imitate). Include fallback instructions: "if no fake exists,
extract the pure functions and test those; do not build a mocking
framework." Note environment constraints (e.g. no `-race` on this
device; CI must run it) and the manual test script.

## N+2. Constraints & non-goals

The contract ("zero persistence is the contract — any write in the new
path is a bug"), forbidden shortcuts, "no new dependencies", and an
explicit non-goals list so the executor doesn't gold-plate.

## N+3. Milestones

M1..Mn, each independently compilable, one semantic commit each, with
example commit messages.

## N+4. Pitfalls checklist (review against this)

Numbered ❑ items. This is the review's rubric: the post-build reviewer
verifies each item against the diff. Every item concrete (rule A4).
Last item is always: "Line numbers in §2 re-located with grep, not
trusted blindly."
