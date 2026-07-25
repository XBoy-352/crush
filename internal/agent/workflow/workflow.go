package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"
)

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

// Options configures workflow execution limits.
type Options struct {
	MaxConcurrent int // <=0 → DefaultMaxConcurrent
	MaxAgents     int // <=0 → DefaultMaxAgents
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

	mu         sync.Mutex
	logs       []string
	agentIndex int

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

func (st *runState) addLog(msg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.logs) == maxLogEntries {
		st.logs = append(st.logs, "(further logs dropped)")
		return
	}
	if len(st.logs) > maxLogEntries {
		return
	}
	if len(msg) > maxLogEntryBytes {
		msg = msg[:maxLogEntryBytes]
	}
	st.logs = append(st.logs, msg)
}

// Run compiles and executes a Lua script. It returns ctx.Err() if
// canceled. Script errors come back as ordinary errors.
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

	L := lua.NewState()
	defer L.Close()
	L.SetContext(ctx)

	st := &runState{
		ctx:   ctx,
		spawn: spawn,
		opts:  opts,
		sem:   make(chan struct{}, opts.MaxConcurrent),
	}

	registerAgent(L, st)
	registerParallel(L, st)
	registerLog(L, st)

	// Wrap the script in a function and call it, so `return` works.
	wrapped := "return function()\n" + script + "\nend"
	fn, err := L.LoadString(wrapped)
	if err != nil {
		return Result{}, fmt.Errorf("script error: %w", err)
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		return Result{}, fmt.Errorf("script error: %w", err)
	}

	// The chunk returns a function; call it.
	if err := L.CallByParam(lua.P{
		Fn:      L.Get(-1),
		NRet:    1,
		Protect: true,
	}, lua.LNil); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("workflow script failed: %w", err)
	}

	ret := L.Get(-1)
	L.Pop(1)

	var val string
	if ret != lua.LNil {
		val = luaValueToJSON(L, ret)
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

// registerAgent registers the `agent(prompt, opts?)` function.
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
		isJSON := false
		if L.GetTop() > 1 {
			opts := L.Get(2)
			if t, ok := opts.(*lua.LTable); ok {
				if l := t.RawGetString("label"); l != lua.LNil {
					label = lua.LVAsString(l)
				}
				if j := t.RawGetString("json"); j != lua.LNil {
					isJSON = lua.LVAsBool(j)
				}
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

		text, err := st.spawn(st.ctx, idx, label, prompt)
		if err != nil {
			L.RaiseError("%s", err.Error())
		}

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

// registerParallel registers the `parallel(calls)` function.
func registerParallel(L *lua.LState, st *runState) {
	L.SetGlobal("parallel", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() == 0 {
			L.RaiseError("parallel() requires an array of calls")
		}
		t := L.CheckTable(1)
		calls := tableToSlice(t)
		if len(calls) == 0 {
			L.RaiseError("parallel() requires a non-empty array of call objects")
		}

		type parsedCall struct {
			index  int
			prompt string
			label  string
			isJSON bool
		}
		parsed := make([]parsedCall, len(calls))

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
			parsed[i] = parsedCall{
				prompt: p,
				label:  label,
				isJSON: isJSON,
			}
		}

		startIdx, err := st.reserveIndices(len(calls))
		if err != nil {
			L.RaiseError("%s", err.Error())
		}
		for i := range parsed {
			parsed[i].index = startIdx + i
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
				select {
				case st.sem <- struct{}{}:
					defer func() { <-st.sem }()
				case <-st.ctx.Done():
					results[i] = spawnResult{err: st.ctx.Err()}
					return
				}
				text, err := st.spawn(st.ctx, pc.index, pc.label, pc.prompt)
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
			} else {
				if parsed[i].isJSON {
					jsTxt, err := extractJSON(res.text)
					if err != nil {
						entry.RawSetString("ok", lua.LFalse)
						entry.RawSetString("error", lua.LString(err.Error()))
						entry.RawSetString("label", lua.LString(parsed[i].label))
					} else {
						entry.RawSetString("ok", lua.LTrue)
						luaVal, _ := jsonToLua(L, jsTxt)
						entry.RawSetString("value", luaVal)
						entry.RawSetString("label", lua.LString(parsed[i].label))
					}
				} else {
					entry.RawSetString("ok", lua.LTrue)
					entry.RawSetString("value", lua.LString(res.text))
					entry.RawSetString("label", lua.LString(parsed[i].label))
				}
			}
			out.RawSetInt(i+1, entry)
		}

		L.Push(out)
		return 1
	}))
}

// registerLog registers the `log(msg)` function.
func registerLog(L *lua.LState, st *runState) {
	L.SetGlobal("log", L.NewFunction(func(L *lua.LState) int {
		if L.GetTop() > 0 {
			st.addLog(lua.LVAsString(L.Get(1)))
		}
		return 0
	}))
}

// tableToSlice converts a Lua table used as a 1-indexed array into a slice.
func tableToSlice(t *lua.LTable) []lua.LValue {
	var out []lua.LValue
	t.ForEach(func(k, v lua.LValue) {
		out = append(out, v)
	})
	return out
}

// luaValueToJSON converts a Lua value to a JSON string.
func luaValueToJSON(L *lua.LState, v lua.LValue) string {
	switch val := v.(type) {
	case lua.LString:
		return `"` + jsonEscape(string(val)) + `"`
	case lua.LNumber:
		return fmt.Sprintf("%v", float64(val))
	case lua.LBool:
		if bool(val) {
			return "true"
		}
		return "false"
	case *lua.LTable:
		// Determine if it's an array or object.
		if isArray(val) {
			return luaTableToJSONArray(L, val)
		}
		return luaTableToJSONObject(L, val)
	default:
		return "null"
	}
}

// isArray returns true if the table is a sequence (1..n).
func isArray(t *lua.LTable) bool {
	if t.Len() == 0 {
		return false
	}
	arr := true
	t.ForEach(func(k, _ lua.LValue) {
		if _, ok := k.(lua.LNumber); !ok {
			arr = false
		}
	})
	return arr
}

func luaTableToJSONArray(L *lua.LState, t *lua.LTable) string {
	var parts []string
	t.ForEach(func(_, v lua.LValue) {
		parts = append(parts, luaValueToJSON(L, v))
	})
	return "[" + strings.Join(parts, ",") + "]"
}

func luaTableToJSONObject(L *lua.LState, t *lua.LTable) string {
	var parts []string
	t.ForEach(func(k, v lua.LValue) {
		key := lua.LVAsString(k)
		parts = append(parts, `"`+jsonEscape(key)+`":`+luaValueToJSON(L, v))
	})
	return "{" + strings.Join(parts, ",") + "}"
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	// b includes surrounding quotes; strip them.
	return string(b[1 : len(b)-1])
}

// jsonToLua parses a JSON string and returns a Lua value.
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

// extractJSON finds a JSON value in a string, handling code fences and
// surrounding prose.
func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "\n")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "\n")
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
