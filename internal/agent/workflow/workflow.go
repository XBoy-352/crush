package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kaptinlin/jsonschema"
	lua "github.com/yuin/gopher-lua"
)

const (
	DefaultMaxConcurrent = 5
	DefaultMaxAgents     = 100
	// DefaultTimeout must leave the agent cap actually reachable: 100 agents at
	// concurrency 5 is 20 sequential batches, and a real subagent turn runs tens
	// of seconds. It is a runaway backstop, not the primary bound -- MaxAgents
	// bounds total work, and the caller's context cancels an interrupted run.
	DefaultTimeout   = 30 * time.Minute
	MaxScriptBytes   = 64 * 1024
	maxLogEntries    = 200
	maxLogEntryBytes = 2048
	maxJSONDepth     = 100
)

// Valid values for the "model" option in agent()/parallel().
var ValidModels = []string{"large", "small"}

// Valid values for the "agent" option in agent()/parallel().
var ValidAgentProfiles = []string{"task", "coder"}

// SpawnOpts carries the per-spawn options extracted from the Lua script.
// The workflow engine passes them through verbatim; the caller
// (workflow_tool.go) resolves them to real agent configs.
type SpawnOpts struct {
	Model string // "large", "small", or "" (inherit profile default).
	Agent string // "task", "coder", or "" (defaults to "task").
}

// Schema wraps a compiled JSON schema used to validate an agent's JSON
// output. It is created by parseSchemaOpt from a `schema` option table.
type Schema struct {
	compiled *jsonschema.Schema
}

// parseSchemaOpt extracts the optional `schema` field (a Lua table holding a
// JSON-schema-shaped table) from spawn options and compiles it. Returns nil
// when no schema is set; raises a Lua error when it cannot be compiled.
func parseSchemaOpt(L *lua.LState, t *lua.LTable, funcName string) *Schema {
	v := t.RawGetString("schema")
	if v == lua.LNil {
		return nil
	}
	schemaT, ok := v.(*lua.LTable)
	if !ok {
		L.RaiseError("%s schema option must be a table", funcName)
	}
	goSchema, err := luaToGo(schemaT, make(map[*lua.LTable]bool), 0)
	if err != nil {
		L.RaiseError("%s invalid schema: %s", funcName, err.Error())
	}
	raw, err := json.Marshal(goSchema)
	if err != nil {
		L.RaiseError("%s could not encode schema: %s", funcName, err.Error())
	}
	compiled, err := jsonschema.NewCompiler().Compile(raw)
	if err != nil {
		L.RaiseError("%s invalid JSON schema: %s", funcName, err.Error())
	}
	return &Schema{compiled: compiled}
}

// SpawnFunc runs one subagent. index is the global 0-based agent index
// (unique per workflow run, used for synthetic tool-call IDs), label an
// optional display title, prompt the task. Returns the agent's final text.
type SpawnFunc func(ctx context.Context, index int, label, prompt string, opts SpawnOpts) (string, error)

// ProgressFunc receives workflow state changes. Optional; nil means no reporting.
// Must be safe to call from multiple goroutines.
type ProgressFunc func(Progress)

// Progress carries a snapshot of workflow engine state at one point in time.
type Progress struct {
	Seq       int64  // monotonic, assigned under the engine lock
	Kind      string // "log" | "agent_start" | "agent_done" | "agent_error"
	Index     int    // agent index, -1 for log events
	Label     string // agent label if set
	Message   string // log text, or error text
	Running   int    // agents currently executing
	Completed int    // agents finished (success or error)
	Total     int    // agents started so far
	Phase     string // current phase set by phase(), "" until then
}

// Progress has a monotonic Seq so out-of-order delivery cannot rewind
// consumers; drop any event whose Seq is not greater than the last applied.

// Options configures workflow execution limits.
type Options struct {
	MaxConcurrent int
	MaxAgents     int
	Timeout       time.Duration
	Progress      ProgressFunc
	// Args is exposed to the script as the global `args` table so a
	// workflow can be parameterized without editing its source. May be nil.
	Args map[string]string
}

// Result is the outcome of a workflow run.
type Result struct {
	Value      string   // JSON-encoded script return value; "" if nil
	Logs       []string // log() lines, capped
	AgentCount int      // agents actually started
}

type runState struct {
	ctx   context.Context
	spawn SpawnFunc
	opts  Options

	mu             sync.Mutex
	logs           []string
	agentIndex     int
	runningCount   int
	completedCount int
	seq            int64
	phase          string

	sem chan struct{}
}

func (st *runState) nextIndex() (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.agentIndex >= st.opts.MaxAgents {
		return 0, fmt.Errorf("workflow agent limit (%d) reached", st.opts.MaxAgents)
	}
	idx := st.agentIndex
	st.agentIndex++
	return idx, nil
}

func (st *runState) reserveIndices(n int) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.agentIndex+n > st.opts.MaxAgents {
		return 0, fmt.Errorf("workflow agent limit (%d) reached", st.opts.MaxAgents)
	}
	start := st.agentIndex
	st.agentIndex += n
	return start, nil
}

func (st *runState) progress(kind string, idx int, label, msg string) {
	st.mu.Lock()
	switch kind {
	case "agent_start":
		st.runningCount++
	case "agent_done", "agent_error":
		st.runningCount--
		st.completedCount++
	}
	st.seq++
	p := Progress{
		Seq:       st.seq,
		Kind:      kind,
		Index:     idx,
		Label:     label,
		Message:   msg,
		Running:   st.runningCount,
		Completed: st.completedCount,
		Total:     st.agentIndex,
		Phase:     st.phase,
	}
	st.mu.Unlock()
	if st.opts.Progress != nil {
		st.opts.Progress(p)
	}
}

func (st *runState) addLog(msg string) {
	st.mu.Lock()
	if len(st.logs) == maxLogEntries {
		st.logs = append(st.logs, "(further logs dropped)")
		st.mu.Unlock()
		return
	}
	if len(st.logs) > maxLogEntries {
		st.mu.Unlock()
		return
	}
	msg = truncateUTF8(msg, maxLogEntryBytes)
	st.logs = append(st.logs, msg)
	st.mu.Unlock()
	st.progress("log", -1, "", msg)
}

// spawnWithSchema runs one subagent and, when schema is non-nil and the raw
// reply carries a JSON value, validates it against the schema. A validation
// failure is returned as an error so callers report it like any spawn error.
// Without a schema it returns the raw text untouched.
func (st *runState) spawnWithSchema(ctx context.Context, index int, label, prompt string, opts SpawnOpts, schema *Schema) (string, error) {
	text, err := st.spawn(ctx, index, label, prompt, opts)
	if err != nil || schema == nil {
		return text, err
	}
	jsTxt, extractErr := extractJSON(text)
	if extractErr != nil {
		return "", fmt.Errorf("schema validation failed: %w", extractErr)
	}
	result := schema.compiled.ValidateJSON([]byte(jsTxt))
	if !result.IsValid() {
		var msgs []string
		for _, e := range result.Errors {
			msgs = append(msgs, e.Error())
		}
		slices.Sort(msgs)
		return "", fmt.Errorf("schema validation failed: %s", strings.Join(msgs, "; "))
	}
	return text, nil
}

// truncateUTF8 caps s at limit bytes without splitting a rune. Slicing a
// UTF-8 string at an arbitrary byte offset can leave a partial sequence, which
// json.Marshal then rewrites to U+FFFD, so back off to the last rune boundary
// at or before the limit.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// snapshot returns the logs and agent count accumulated so far. Callers use it
// on failure paths so a workflow that dies mid-run still reports the log()
// output produced before the error -- which is exactly what the model needs to
// diagnose the failure.
func (st *runState) snapshot() Result {
	st.mu.Lock()
	defer st.mu.Unlock()
	return Result{Logs: st.logs, AgentCount: st.agentIndex}
}

// Run compiles and executes a Lua script. It returns ctx.Err() if
// canceled. Script errors come back as ordinary errors. On failure the
// returned Result still carries the logs and agent count accumulated
// before the error.
func Run(ctx context.Context, script string, spawn SpawnFunc, opts Options) (Result, error) {
	if len(script) > MaxScriptBytes {
		return Result{}, fmt.Errorf("script exceeds maximum length of %d bytes", MaxScriptBytes)
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.MaxAgents <= 0 {
		opts.MaxAgents = DefaultMaxAgents
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	L := newSandboxedState()
	defer L.Close()
	L.SetContext(runCtx)

	st := &runState{
		ctx:   runCtx,
		spawn: spawn,
		opts:  opts,
		sem:   make(chan struct{}, opts.MaxConcurrent),
	}

	if len(opts.Args) > 0 {
		args := L.NewTable()
		for k, v := range opts.Args {
			args.RawSetString(k, lua.LString(v))
		}
		L.SetGlobal("args", args)
	}

	registerAgent(L, st)
	registerParallel(L, st)
	registerLog(L, st)
	registerPhase(L, st)
	registerPipeline(L, st)

	// Wrap so bare `return` works. The comment keeps script line numbers
	// aligned (wrapper line 1 maps to script line 1 after offset strip).
	wrapped := "return function() -- line 1\n" + script + "\nend"
	fn, err := L.LoadString(wrapped)
	if err != nil {
		if runCtx.Err() != nil {
			return st.snapshot(), runCtx.Err()
		}
		return st.snapshot(), fmt.Errorf("script error: %w", stripLineOffset(err))
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		if runCtx.Err() != nil {
			return st.snapshot(), runCtx.Err()
		}
		return st.snapshot(), fmt.Errorf("script error: %w", stripLineOffset(err))
	}

	if err := L.CallByParam(lua.P{
		Fn:      L.Get(-1),
		NRet:    1,
		Protect: true,
	}, lua.LNil); err != nil {
		if runCtx.Err() != nil {
			return st.snapshot(), runCtx.Err()
		}
		return st.snapshot(), fmt.Errorf("workflow script failed: %w", stripLineOffset(err))
	}

	ret := L.Get(-1)
	L.Pop(1)

	var val string
	if ret != lua.LNil {
		tree, err := luaToGo(ret, make(map[*lua.LTable]bool), 0)
		if err != nil {
			return st.snapshot(), fmt.Errorf("failed to serialize return value: %w", err)
		}
		b, err := json.Marshal(tree)
		if err != nil {
			return st.snapshot(), fmt.Errorf("could not marshal return value: %w", err)
		}
		val = string(b)
	}

	st.mu.Lock()
	logs := st.logs
	agentCount := st.agentIndex
	st.mu.Unlock()

	return Result{
		Value:      val,
		Logs:       logs,
		AgentCount: agentCount,
	}, nil
}

// newSandboxedState creates a Lua state with only safe stdlib libraries:
// base, table, string, math. It does not load os, io, package, or debug.
func newSandboxedState() *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		if err := L.CallByParam(lua.P{
			Fn:      L.NewFunction(lib.fn),
			NRet:    0,
			Protect: true,
		}, lua.LString(lib.name)); err != nil {
			L.Close()
			panic(fmt.Sprintf("failed to open %s lib: %v", lib.name, err))
		}
	}
	for _, g := range []string{
		"dofile", "loadfile", "load", "loadstring", "print", "collectgarbage",
		// OpenBase registers these as globals even without package/os libs.
		"require", "module", "package", "os", "io", "debug", "coroutine",
	} {
		L.SetGlobal(g, lua.LNil)
	}
	return L
}

var (
	// Runtime errors: `<string>:4: message`.
	reRuntimeLine = regexp.MustCompile(`<string>:(\d+):`)
	// Syntax errors from LoadString: `<string> line:4(column:11) near ...`.
	reSyntaxLine = regexp.MustCompile(`<string> line:(\d+)\(`)
)

// stripLineOffset adjusts error line numbers for the wrapper line Run prepends
// to the script, so reported lines match the script the caller wrote. Only the
// two shapes gopher-lua actually emits are rewritten; any other message is
// returned untouched rather than spliced blindly on its colons.
func stripLineOffset(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, re := range []*regexp.Regexp{reRuntimeLine, reSyntaxLine} {
		loc := re.FindStringSubmatchIndex(msg)
		if loc == nil {
			continue
		}
		lineNum, convErr := strconv.Atoi(msg[loc[2]:loc[3]])
		if convErr != nil {
			continue
		}
		adjusted := lineNum - 1
		if adjusted < 1 {
			adjusted = 1
		}
		return errors.New(msg[:loc[2]] + strconv.Itoa(adjusted) + msg[loc[3]:])
	}
	return err
}

func registerAgent(L *lua.LState, st *runState) {
	L.SetGlobal("agent", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() == 0 {
			L.RaiseError("agent() requires a prompt")
		}
		prompt := L.CheckString(1)
		if prompt == "" {
			L.RaiseError("agent() prompt cannot be empty")
		}

		var label string
		var spawnOpts SpawnOpts
		isJSON := false
		var schema *Schema
		if L.GetTop() > 1 {
			opts := L.Get(2)
			if t, ok := opts.(*lua.LTable); ok {
				if l := t.RawGetString("label"); l != lua.LNil {
					label = lua.LVAsString(l)
				}
				if j := t.RawGetString("json"); j != lua.LNil {
					isJSON = lua.LVAsBool(j)
				}
				schema = parseSchemaOpt(L, t, "agent()")
				spawnOpts = parseSpawnOpts(L, t)
			}
		}

		idx, err := st.nextIndex()
		if err != nil {
			L.RaiseError("%s", err.Error())
		}

		select {
		case st.sem <- struct{}{}:
			defer func() { <-st.sem }()
		case <-st.ctx.Done():
			L.RaiseError("%s", st.ctx.Err().Error())
		}

		st.progress("agent_start", idx, label, "")

		text, err := st.spawnWithSchema(st.ctx, idx, label, prompt, spawnOpts, schema)
		if err != nil {
			st.progress("agent_error", idx, label, err.Error())
			L.RaiseError("%s", err.Error())
		}

		st.progress("agent_done", idx, label, "")

		if isJSON {
			parsed, err := extractJSON(text)
			if err != nil {
				L.RaiseError("%s", err.Error())
			}
			result, err := jsonToLua(L, parsed)
			if err != nil {
				L.RaiseError("%s", err.Error())
			}
			L.Push(result)
			return 1
		}

		L.Push(lua.LString(text))
		return 1
	}))
}

// parseSpawnOpts extracts and validates the model/agent options from a
// Lua options table. It raises a Lua error on unknown values.
func parseSpawnOpts(L *lua.LState, t *lua.LTable) SpawnOpts {
	var opts SpawnOpts
	if v := t.RawGetString("model"); v != lua.LNil {
		opts.Model = lua.LVAsString(v)
		if !slices.Contains(ValidModels, opts.Model) {
			L.RaiseError("invalid model %q; valid values: %s", opts.Model, strings.Join(ValidModels, ", "))
		}
	}
	if v := t.RawGetString("agent"); v != lua.LNil {
		opts.Agent = lua.LVAsString(v)
		if !slices.Contains(ValidAgentProfiles, opts.Agent) {
			L.RaiseError("invalid agent %q; valid values: %s", opts.Agent, strings.Join(ValidAgentProfiles, ", "))
		}
	}
	return opts
}

func registerParallel(L *lua.LState, st *runState) {
	L.SetGlobal("parallel", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() == 0 {
			L.RaiseError("parallel() requires an array of calls")
		}
		t := L.CheckTable(1)
		calls, err := tableToSlice(t)
		if err != nil {
			L.RaiseError("parallel() argument: %s", err.Error())
		}
		// Reject stray named keys (e.g. parallel({...}, timeout=30))
		// deterministically; ForEach would have appended them at a random
		// position in Go map order.
		keyCount := 0
		t.ForEach(func(_, _ lua.LValue) { keyCount++ })
		if keyCount > len(calls) {
			L.RaiseError("parallel() expects an array of call objects, got a table with named keys")
		}
		if len(calls) == 0 {
			L.RaiseError("parallel() requires a non-empty array of call objects")
		}

		type parsedCall struct {
			index     int
			prompt    string
			label     string
			isJSON    bool
			spawnOpts SpawnOpts
			schema    *Schema
		}
		parsed := make([]parsedCall, len(calls))
		hasCoder := false

		for i, c := range calls {
			m, ok := c.(*lua.LTable)
			if !ok {
				L.RaiseError("parallel() call at index %d is not an object", i)
			}
			p := lua.LVAsString(m.RawGetString("prompt"))
			if p == "" {
				L.RaiseError("parallel() call at index %d is missing a prompt string", i)
			}
			var label string
			if l := m.RawGetString("label"); l != lua.LNil {
				label = lua.LVAsString(l)
			}
			var isJSON bool
			if j := m.RawGetString("json"); j != lua.LNil {
				isJSON = lua.LVAsBool(j)
			}
			so := parseSpawnOpts(L, m)
			if so.Agent == "coder" {
				hasCoder = true
			}
			parsed[i] = parsedCall{
				prompt:    p,
				label:     label,
				isJSON:    isJSON,
				spawnOpts: so,
				schema:    parseSchemaOpt(L, m, "parallel()"),
			}
		}

		startIdx, err := st.reserveIndices(len(calls))
		if err != nil {
			L.RaiseError("%s", err.Error())
		}
		for i := range parsed {
			parsed[i].index = startIdx + i
		}

		// Safety 3b: if any entry requests agent="coder", force
		// serialisation to prevent concurrent writers corrupting
		// files in the shared working tree. Worktree isolation is
		// the better long-term answer but out of scope.
		var batchSem chan struct{}
		if hasCoder {
			batchSem = make(chan struct{}, 1)
		}

		type spawnResult struct {
			text string
			err  error
		}
		results := make([]spawnResult, len(calls))
		var wg sync.WaitGroup
		for i, pc := range parsed {
			wg.Add(1)
			go func(i int, pc parsedCall) {
				defer wg.Done()

				// Coder entries serialise against each other (Safety 3b:
				// concurrent writers corrupt the shared working tree). Acquire
				// the batch lock BEFORE the global semaphore so a blocked coder
				// does not sit on a global slot and starve read-only tasks.
				// Read-only "task" agents never touch the working tree and are
				// not gated here.
				if batchSem != nil && pc.spawnOpts.Agent == "coder" {
					select {
					case batchSem <- struct{}{}:
						defer func() { <-batchSem }()
					case <-st.ctx.Done():
						results[i] = spawnResult{err: st.ctx.Err()}
						return
					}
				}

				// Acquire the global concurrency semaphore.
				select {
				case st.sem <- struct{}{}:
					defer func() { <-st.sem }()
				case <-st.ctx.Done():
					results[i] = spawnResult{err: st.ctx.Err()}
					return
				}

				st.progress("agent_start", pc.index, pc.label, "")

				text, err := st.spawnWithSchema(st.ctx, pc.index, pc.label, pc.prompt, pc.spawnOpts, pc.schema)
				if err != nil {
					st.progress("agent_error", pc.index, pc.label, err.Error())
				} else {
					st.progress("agent_done", pc.index, pc.label, "")
				}
				results[i] = spawnResult{text: text, err: err}
			}(i, pc)
		}
		wg.Wait()

		out := L.NewTable()
		for i, res := range results {
			entry := L.NewTable()
			if res.err != nil {
				entry.RawSetString("ok", lua.LFalse)
				entry.RawSetString("error", lua.LString(res.err.Error()))
				entry.RawSetString("label", lua.LString(parsed[i].label))
			} else if parsed[i].isJSON {
				jsTxt, err := extractJSON(res.text)
				if err != nil {
					entry.RawSetString("ok", lua.LFalse)
					entry.RawSetString("error", lua.LString(err.Error()))
					entry.RawSetString("label", lua.LString(parsed[i].label))
				} else {
					luaVal, err := jsonToLua(L, jsTxt)
					if err != nil {
						entry.RawSetString("ok", lua.LFalse)
						entry.RawSetString("error", lua.LString(err.Error()))
					} else {
						entry.RawSetString("ok", lua.LTrue)
						entry.RawSetString("value", luaVal)
					}
					entry.RawSetString("label", lua.LString(parsed[i].label))
				}
			} else {
				entry.RawSetString("ok", lua.LTrue)
				entry.RawSetString("value", lua.LString(res.text))
				entry.RawSetString("label", lua.LString(parsed[i].label))
			}
			out.RawSetInt(i+1, entry)
		}

		L.Push(out)
		return 1
	}))
}

func registerLog(L *lua.LState, st *runState) {
	L.SetGlobal("log", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() > 0 {
			st.addLog(lua.LVAsString(L.Get(1)))
		}
		return 0
	}))
}

// registerPhase adds the phase() builtin: a display-only marker that groups
// subsequent agents in progress consumers (e.g. the workflow popup header).
func registerPhase(L *lua.LState, st *runState) {
	L.SetGlobal("phase", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if name == "" {
			L.RaiseError("phase() name cannot be empty")
		}
		st.mu.Lock()
		st.phase = name
		st.mu.Unlock()
		return 0
	}))
}

// registerPipeline adds the pipeline(items, stages) builtin.
//
// ponytail: stage output is string-typed (the raw agent reply), there is no
// barrier or cross-item dependency support, and stages are plain spec tables
// rather than Lua callbacks -- gopher-lua's agent() blocks the VM, so a
// subagent can never call back into the script to run a stage function. The
// upgrade path if richer staging is ever needed is a native Go orchestrator,
// not more Lua machinery.
func registerPipeline(L *lua.LState, st *runState) {
	L.SetGlobal("pipeline", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() < 2 {
			L.RaiseError("pipeline() requires items and stages tables")
		}
		itemsT := L.CheckTable(1)
		stagesT := L.CheckTable(2)

		items, err := tableToSlice(itemsT)
		if err != nil {
			L.RaiseError("pipeline() items: %s", err.Error())
		}
		keyCount := 0
		itemsT.ForEach(func(_, _ lua.LValue) { keyCount++ })
		if keyCount > len(items) {
			L.RaiseError("pipeline() expects an array of items, got a table with named keys")
		}
		if len(items) == 0 {
			L.RaiseError("pipeline() requires a non-empty array of items")
		}

		rawStages, err := tableToSlice(stagesT)
		if err != nil {
			L.RaiseError("pipeline() stages: %s", err.Error())
		}
		keyCount = 0
		stagesT.ForEach(func(_, _ lua.LValue) { keyCount++ })
		if keyCount > len(rawStages) {
			L.RaiseError("pipeline() expects an array of stage objects, got a table with named keys")
		}
		if len(rawStages) == 0 {
			L.RaiseError("pipeline() requires a non-empty array of stages")
		}

		type stage struct {
			prompt    string
			label     string
			isJSON    bool
			spawnOpts SpawnOpts
			schema    *Schema
		}
		stages := make([]stage, len(rawStages))
		for i, s := range rawStages {
			specT, ok := s.(*lua.LTable)
			if !ok {
				L.RaiseError("pipeline() stage at index %d is not an object", i+1)
			}
			p := lua.LVAsString(specT.RawGetString("prompt"))
			if p == "" {
				L.RaiseError("pipeline() stage at index %d is missing a prompt string", i+1)
			}
			var label string
			if l := specT.RawGetString("label"); l != lua.LNil {
				label = lua.LVAsString(l)
			}
			var isJSON bool
			if j := specT.RawGetString("json"); j != lua.LNil {
				isJSON = lua.LVAsBool(j)
			}
			schema := parseSchemaOpt(L, specT, "pipeline()")
			stages[i] = stage{
				prompt:    p,
				label:     label,
				isJSON:    isJSON,
				spawnOpts: parseSpawnOpts(L, specT),
				schema:    schema,
			}
		}

		startIdx, err := st.reserveIndices(len(items) * len(stages))
		if err != nil {
			L.RaiseError("%s", err.Error())
		}

		type spawnResult struct {
			text string
			err  error
		}
		results := make([]spawnResult, len(items))
		var wg sync.WaitGroup
		for i, item := range items {
			wg.Add(1)
			go func(i int, item lua.LValue) {
				defer wg.Done()

				itemStr := lua.LVAsString(item)
				output := ""
				for j, sg := range stages {
					// Acquire the semaphore per stage so concurrency is
					// bounded by live work, not by items holding slots
					// while waiting on earlier stages of their own chain.
					select {
					case st.sem <- struct{}{}:
						defer func() { <-st.sem }()
					case <-st.ctx.Done():
						results[i] = spawnResult{err: st.ctx.Err()}
						return
					}

					prompt := strings.ReplaceAll(sg.prompt, "{{item}}", itemStr)
					prompt = strings.ReplaceAll(prompt, "{{output}}", output)

					idx := startIdx + i*len(stages) + j
					label := sg.label
					if label == "" {
						label = fmt.Sprintf("Pipeline %d stage %d/%d", i+1, j+1, len(stages))
					}

					st.progress("agent_start", idx, label, "")

					text, err := st.spawnWithSchema(st.ctx, idx, label, prompt, sg.spawnOpts, sg.schema)
					if err != nil {
						st.progress("agent_error", idx, label, err.Error())
						results[i] = spawnResult{err: fmt.Errorf("stage %d/%d failed for item %q: %w", j+1, len(stages), itemStr, err)}
						return
					}
					st.progress("agent_done", idx, label, "")
					output = text
				}
				results[i] = spawnResult{text: output}
			}(i, item)
		}
		wg.Wait()

		out := L.NewTable()
		for i, res := range results {
			entry := L.NewTable()
			itemStr := lua.LVAsString(items[i])
			if res.err != nil {
				entry.RawSetString("ok", lua.LFalse)
				entry.RawSetString("error", lua.LString(res.err.Error()))
				entry.RawSetString("item", lua.LString(itemStr))
			} else {
				value := lua.LValue(lua.LString(res.text))
				if stages[len(stages)-1].isJSON {
					jsTxt, err := extractJSON(res.text)
					if err != nil {
						entry.RawSetString("ok", lua.LFalse)
						entry.RawSetString("error", lua.LString(err.Error()))
					} else {
						luaVal, err := jsonToLua(L, jsTxt)
						if err != nil {
							entry.RawSetString("ok", lua.LFalse)
							entry.RawSetString("error", lua.LString(err.Error()))
						} else {
							entry.RawSetString("ok", lua.LTrue)
							entry.RawSetString("value", luaVal)
						}
					}
				} else {
					entry.RawSetString("ok", lua.LTrue)
					entry.RawSetString("value", value)
				}
				entry.RawSetString("item", lua.LString(itemStr))
			}
			out.RawSetInt(i+1, entry)
		}

		L.Push(out)
		return 1
	}))
}

// tableToSlice returns the 1..MaxN array part of t in index order.
//
// gopher-lua's ForEach walks the array part in order but then appends the
// string and hash parts in Go map order, which silently turns a stray named
// key into an extra positional entry at a random position. Iterating by
// index instead makes both a hole and a stray key a deterministic error
// rather than a confusing downstream one.
func tableToSlice(t *lua.LTable) ([]lua.LValue, error) {
	n := t.MaxN()
	out := make([]lua.LValue, 0, n)
	for i := 1; i <= n; i++ {
		v := t.RawGetInt(i)
		if v == lua.LNil {
			return nil, fmt.Errorf("array has a nil hole at index %d", i)
		}
		out = append(out, v)
	}
	return out, nil
}

// luaToGo converts a Lua value to a Go any suitable for json.Marshal.
// Includes cycle detection and a depth cap.
func luaToGo(v lua.LValue, seen map[*lua.LTable]bool, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, errors.New("table nesting exceeds maximum depth")
	}
	switch val := v.(type) {
	case lua.LString:
		return string(val), nil
	case lua.LNumber:
		n := float64(val)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("cannot serialize NaN or Inf as JSON")
		}
		return n, nil
	case lua.LBool:
		return bool(val), nil
	case *lua.LTable:
		if seen[val] {
			return nil, errors.New("cannot serialize cyclic table")
		}
		seen[val] = true
		defer delete(seen, val)

		if isArrayTable(val) {
			arr := make([]any, 0, val.Len())
			for i := 1; i <= val.Len(); i++ {
				elem, err := luaToGo(val.RawGetInt(i), seen, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, elem)
			}
			return arr, nil
		}
		m := map[string]any{}
		var convertErr error
		val.ForEach(func(k, child lua.LValue) {
			if convertErr != nil {
				return
			}
			converted, err := luaToGo(child, seen, depth+1)
			if err != nil {
				convertErr = err
				return
			}
			m[lua.LVAsString(k)] = converted
		})
		if convertErr != nil {
			return nil, convertErr
		}
		return m, nil
	default:
		return nil, nil
	}
}

// isArrayTable returns true if the table is a true sequence: every key a
// positive integer with no holes. Sparse or non-integer numeric keys ({[0]=x},
// {[5]=x}, {[1.5]=x}) serialize as objects instead, since the array branch
// walks 1..Len() and would drop those values or pad the gaps with nulls.
// Empty tables are sequences, so they serialize as [].
func isArrayTable(t *lua.LTable) bool {
	n := 0
	isSeq := true
	t.ForEach(func(k, _ lua.LValue) {
		num, ok := k.(lua.LNumber)
		if !ok || float64(num) != math.Trunc(float64(num)) || num < 1 {
			isSeq = false
		}
		n++
	})
	// n == t.Len() rejects holes; both are 0 for an empty table.
	return isSeq && n == t.Len()
}

func jsonToLua(L *lua.LState, s string) (lua.LValue, error) {
	var raw any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return lua.LNil, err
	}
	return goValueToLua(L, raw), nil
}

func goValueToLua(L *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case float64:
		return lua.LNumber(val)
	case string:
		return lua.LString(val)
	case []any:
		t := L.NewTable()
		for i, e := range val {
			t.RawSetInt(i+1, goValueToLua(L, e))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range val {
			t.RawSetString(k, goValueToLua(L, e))
		}
		return t
	default:
		return lua.LNil
	}
}

func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)

	if prefix, ok := strings.CutPrefix(s, "```json"); ok {
		s = strings.TrimSpace(prefix)
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if prefix, ok := strings.CutPrefix(s, "```"); ok {
		s = strings.TrimSpace(prefix)
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	var dummy any
	if err := json.Unmarshal([]byte(s), &dummy); err == nil {
		return s, nil
	}

	firstBrace := strings.IndexAny(s, "{[")
	lastBrace := strings.LastIndexAny(s, "}]")
	if firstBrace != -1 && lastBrace != -1 && lastBrace > firstBrace {
		sub := s[firstBrace : lastBrace+1]
		if err := json.Unmarshal([]byte(sub), &dummy); err == nil {
			return sub, nil
		}
	}

	return "", errors.New("no JSON value found in agent reply")
}
