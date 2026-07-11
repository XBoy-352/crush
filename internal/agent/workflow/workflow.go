package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dop251/goja"
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

type Options struct {
	MaxConcurrent int // <=0 → DefaultMaxConcurrent
	MaxAgents     int // <=0 → DefaultMaxAgents
}

type Result struct {
	Value      string   // JSON-encoded script return value; "" if undefined/null
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

// Run compiles and executes script. It returns ctx.Err() if canceled.
// Script exceptions and syntax errors come back as ordinary errors with
// the JS message preserved.
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

	vm := goja.New()

	st := &runState{
		ctx:   ctx,
		spawn: spawn,
		opts:  opts,
		sem:   make(chan struct{}, opts.MaxConcurrent),
	}

	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-watchdogDone:
		}
	}()

	vm.Set("agent", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.ToValue("agent() requires a prompt"))
		}
		prompt := call.Arguments[0].String()
		if prompt == "" {
			panic(vm.ToValue("agent() prompt cannot be empty"))
		}

		var label string
		var isJSON bool
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			optsObj := call.Arguments[1].ToObject(vm)
			if l := optsObj.Get("label"); l != nil && !goja.IsUndefined(l) {
				label = l.String()
			}
			if j := optsObj.Get("json"); j != nil && !goja.IsUndefined(j) {
				isJSON = j.ToBoolean()
			}
		}

		idx, err := st.nextIndex()
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}

		select {
		case st.sem <- struct{}{}:
			defer func() { <-st.sem }()
		case <-st.ctx.Done():
			panic(vm.ToValue(st.ctx.Err().Error()))
		}

		text, err := st.spawn(st.ctx, idx, label, prompt)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}

		if isJSON {
			parsed, err := extractJSON(text)
			if err != nil {
				panic(vm.ToValue(err.Error()))
			}
			return toJSValue(vm, json.RawMessage(parsed))
		}

		return vm.ToValue(text)
	})

	vm.Set("parallel", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.ToValue("parallel() requires an array of calls"))
		}
		exp := call.Arguments[0].Export()
		calls, ok := exp.([]any)
		if !ok || len(calls) == 0 {
			panic(vm.ToValue("parallel() requires a non-empty array of call objects"))
		}

		type parsedCall struct {
			index  int
			prompt string
			label  string
			isJSON bool
		}
		parsed := make([]parsedCall, len(calls))

		for i, c := range calls {
			m, ok := c.(map[string]any)
			if !ok {
				panic(vm.ToValue(fmt.Sprintf("parallel() call at index %d is not an object", i)))
			}
			p, ok := m["prompt"].(string)
			if !ok || p == "" {
				panic(vm.ToValue(fmt.Sprintf("parallel() call at index %d is missing a prompt string", i)))
			}
			var label string
			if l, ok := m["label"].(string); ok {
				label = l
			}
			var isJSON bool
			if j, ok := m["json"].(bool); ok {
				isJSON = j
			}
			parsed[i] = parsedCall{
				prompt: p,
				label:  label,
				isJSON: isJSON,
			}
		}

		startIdx, err := st.reserveIndices(len(calls))
		if err != nil {
			panic(vm.ToValue(err.Error()))
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

		out := make([]any, len(calls))
		for i, res := range results {
			if res.err != nil {
				out[i] = map[string]any{
					"ok":    false,
					"error": res.err.Error(),
					"label": parsed[i].label,
				}
			} else {
				if parsed[i].isJSON {
					jsTxt, err := extractJSON(res.text)
					if err != nil {
						out[i] = map[string]any{
							"ok":    false,
							"error": err.Error(),
							"label": parsed[i].label,
						}
					} else {
						out[i] = map[string]any{
							"ok":    true,
							"value": json.RawMessage(jsTxt),
							"label": parsed[i].label,
						}
					}
				} else {
					out[i] = map[string]any{
						"ok":    true,
						"value": res.text,
						"label": parsed[i].label,
					}
				}
			}
		}

		return toJSValue(vm, out)
	})

	vm.Set("log", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			st.addLog(call.Arguments[0].String())
		}
		return goja.Undefined()
	})

	prog, err := goja.Compile("workflow.js", "(function(){\n"+script+"\n})()", true)
	if err != nil {
		return Result{}, fmt.Errorf("script error: %w", err)
	}

	v, err := vm.RunProgram(prog)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("workflow script failed: %w", err)
	}

	var val string
	if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		exp := v.Export()
		if _, isPromise := exp.(*goja.Promise); isPromise {
			return Result{}, errors.New("script returned a Promise; async/await is not supported")
		}
		b, err := json.Marshal(exp)
		if err != nil {
			return Result{}, fmt.Errorf("could not marshal return value: %w", err)
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

func toJSValue(vm *goja.Runtime, goValue any) goja.Value {
	b, err := json.Marshal(goValue)
	if err != nil {
		panic(vm.ToValue("could not marshal Go value to JSON: " + err.Error()))
	}

	jsonObj := vm.Get("JSON").ToObject(vm)
	parseFunc, ok := goja.AssertFunction(jsonObj.Get("parse"))
	if !ok {
		panic(vm.ToValue("JSON.parse is not a function"))
	}

	val, err := parseFunc(goja.Undefined(), vm.ToValue(string(b)))
	if err != nil {
		panic(vm.ToValue("JSON.parse failed: " + err.Error()))
	}
	return val
}

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
