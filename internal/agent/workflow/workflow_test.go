package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestWorkflowRun_Basic(t *testing.T) {
	t.Parallel()
	script := `
		local res = agent("hello")
		log("got " .. res)
		return { result = res }
	`
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		require.Equal(t, "hello", prompt)
		return "world", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)
	require.JSONEq(t, `{"result":"world"}`, res.Value)
	require.Equal(t, []string{"got world"}, res.Logs)
	require.Equal(t, 1, res.AgentCount)
}

func TestWorkflowRun_ReturnTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		script string
		val    string
	}{
		{"return 1", "1"},
		{"return 'foo'", `"foo"`},
		{"return {1, 2}", "[1,2]"},
		{"return {a = 1}", `{"a":1}`},
		{"return nil", ""},
	}
	for _, tc := range cases {
		t.Run(tc.script, func(t *testing.T) {
			t.Parallel()
			res, err := Run(context.Background(), tc.script, nil, Options{})
			require.NoError(t, err)
			if tc.val == "" {
				require.Empty(t, res.Value)
			} else {
				require.JSONEq(t, tc.val, res.Value)
			}
		})
	}
}

func TestWorkflowRun_EmptyTable(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), "return {}", nil, Options{})
	require.NoError(t, err)
	require.JSONEq(t, "[]", res.Value)
}

func TestWorkflowRun_AgentErrors(t *testing.T) {
	t.Parallel()
	t.Run("empty prompt throws", func(t *testing.T) {
		t.Parallel()
		script := `
			local ok, err = pcall(function()
				agent("")
			end)
			if ok then return "fail" end
			return err
		`
		res, err := Run(context.Background(), script, nil, Options{})
		require.NoError(t, err)
		require.Contains(t, res.Value, "prompt cannot be empty")
	})

	t.Run("spawn error throws", func(t *testing.T) {
		t.Parallel()
		script := `
			local ok, err = pcall(function()
				agent("foo")
			end)
			if ok then return "fail" end
			return err
		`
		spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
			return "", errors.New("boom")
		}
		res, err := Run(context.Background(), script, spawn, Options{})
		require.NoError(t, err)
		require.Contains(t, res.Value, "boom")
	})
}

func TestWorkflowRun_Parallel(t *testing.T) {
	t.Parallel()
	script := `
		local res = parallel({
			{prompt = "a", label = "l1"},
			{prompt = "b"}
		})
		return res
	`
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		assert.NotEmpty(t, prompt)
		if prompt == "a" {
			assert.Equal(t, "l1", label)
			return "A", nil
		}
		if prompt == "b" {
			assert.Equal(t, "", label)
			return "", errors.New("B_ERR")
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)
	require.Equal(t, 2, res.AgentCount)

	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Value), &arr))
	require.Len(t, arr, 2)

	require.Equal(t, true, arr[0]["ok"])
	require.Equal(t, "A", arr[0]["value"])
	require.Equal(t, "l1", arr[0]["label"])

	require.Equal(t, false, arr[1]["ok"])
	require.Equal(t, "B_ERR", arr[1]["error"])
}

func TestWorkflowRun_ParallelValidation(t *testing.T) {
	t.Parallel()
	script := `
		local ok, err = pcall(function()
			parallel({{label = "no prompt"}})
		end)
		if ok then return "fail" end
		return err
	`
	var called atomic.Bool
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		called.Store(true)
		return "", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)
	require.Contains(t, res.Value, "missing a prompt string")
	require.False(t, called.Load())
}

func TestWorkflowRun_Concurrency(t *testing.T) {
	t.Parallel()
	script := `
		local calls = {}
		for i = 1, 10 do
			calls[i] = {prompt = "p" .. i}
		end
		parallel(calls)
	`
	var active int32
	var maxActive int32
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		v := atomic.AddInt32(&active, 1)
		for {
			cur := atomic.LoadInt32(&maxActive)
			if v <= cur || atomic.CompareAndSwapInt32(&maxActive, cur, v) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return "ok", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{MaxConcurrent: 3, MaxAgents: 20})
	require.NoError(t, err)
	require.Equal(t, 10, res.AgentCount)
	max := int(atomic.LoadInt32(&maxActive))
	require.LessOrEqual(t, max, 3)
	require.GreaterOrEqual(t, max, 2)
}

func TestWorkflowRun_Caps(t *testing.T) {
	t.Parallel()
	script := `
		local i = 0
		local ok, err = pcall(function()
			while i < 200 do
				agent("foo")
				i = i + 1
			end
		end)
		if not ok then log(err) end
		for j = 1, 250 do
			log("log" .. j)
		end
		return i
	`
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		return "ok", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{MaxAgents: 5})
	require.NoError(t, err)
	require.JSONEq(t, "5", res.Value)
	require.Equal(t, 5, res.AgentCount)

	require.Len(t, res.Logs, 201)
	require.Contains(t, res.Logs[0], "limit (5) reached")
	require.Equal(t, "(further logs dropped)", res.Logs[200])
}

func TestWorkflowRun_Cancellation(t *testing.T) {
	t.Parallel()
	script := `while true do end`
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, script, nil, Options{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestWorkflowRun_JSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{"bare", `{"a":1}`, `{"a":1}`, false},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"wrapped", "Here is it:\n```json\n{\"a\":1}\n```\nDone", `{"a":1}`, false},
		{"garbage", "not json at all", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			val, err := extractJSON(tc.output)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.JSONEq(t, tc.want, val)
			}
		})
	}
}

func TestWorkflowRun_AgentJSON(t *testing.T) {
	t.Parallel()
	script := `
		local res = agent("foo", {json = true})
		return res
	`
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		return "```json\n{\"foo\":\"bar\"}\n```", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)
	require.JSONEq(t, `{"foo":"bar"}`, res.Value)
}

func TestWorkflowRun_MaxScript(t *testing.T) {
	t.Parallel()
	script := strings.Repeat("a", MaxScriptBytes+1)
	_, err := Run(context.Background(), script, nil, Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum length")
}

func TestWorkflowRun_LoopUntilDry(t *testing.T) {
	t.Parallel()
	script := `
		local items = {}
		for i = 1, 3 do
			local result = agent("Find the next page of results.", {json = true})
			if not result or #result == 0 then break end
			for _, v in ipairs(result) do
				items[#items+1] = v
			end
		end
		return items
	`
	callCount := 0
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		callCount++
		if callCount == 1 {
			return `["a", "b"]`, nil
		}
		if callCount == 2 {
			return `["c"]`, nil
		}
		return `[]`, nil
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)
	require.JSONEq(t, `["a","b","c"]`, res.Value)
	require.Equal(t, 3, res.AgentCount)
}

func TestWorkflowRun_Sandbox(t *testing.T) {
	t.Parallel()
	script := `
		return {
			type_os = type(os),
			type_io = type(io),
			type_require = type(require),
			type_debug = type(debug),
			type_dofile = type(dofile),
			type_loadfile = type(loadfile),
			type_print = type(print),
			type_package = type(package),
		}
	`
	res, err := Run(context.Background(), script, nil, Options{})
	require.NoError(t, err)

	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(res.Value), &m))
	for _, k := range []string{"type_os", "type_io", "type_require", "type_debug", "type_dofile", "type_loadfile", "type_print", "type_package"} {
		require.Equal(t, "nil", m[k], k)
	}
}

func TestWorkflowRun_CyclicTable(t *testing.T) {
	t.Parallel()
	script := `
		local t = {}
		t.self = t
		return t
	`
	_, err := Run(context.Background(), script, nil, Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cyclic")
}

func TestWorkflowRun_NaN(t *testing.T) {
	t.Parallel()
	script := `return {bad = 0/0}`
	_, err := Run(context.Background(), script, nil, Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "NaN")
}

func TestWorkflowRun_Inf(t *testing.T) {
	t.Parallel()
	script := `return {inf = 1/0}`
	_, err := Run(context.Background(), script, nil, Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Inf")
}

func TestWorkflowRun_Timeout(t *testing.T) {
	t.Parallel()
	script := `while true do end`
	start := time.Now()
	_, err := Run(context.Background(), script, nil, Options{Timeout: 100 * time.Millisecond})
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestWorkflowRun_ParallelJSONExtractionError(t *testing.T) {
	t.Parallel()
	script := `
		local res = parallel({
			{prompt = "a", json = true}
		})
		return res
	`
	spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
		return "not json at all", nil
	}
	res, err := Run(context.Background(), script, spawn, Options{})
	require.NoError(t, err)

	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Value), &arr))
	require.Len(t, arr, 1)
	require.Equal(t, false, arr[0]["ok"])
	require.Contains(t, arr[0]["error"].(string), "no JSON")
}
