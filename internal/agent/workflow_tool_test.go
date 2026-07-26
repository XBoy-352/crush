package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestStripWorkflowTool_RemovesWorkflow asserts safety 3a: the
// workflow tool is stripped from a subagent's allowed tools.
func TestStripWorkflowTool_RemovesWorkflow(t *testing.T) {
	t.Parallel()
	agent := config.Agent{
		AllowedTools: []string{"bash", "edit", "workflow", "view", "grep"},
	}
	stripped := stripWorkflowTool(agent)
	require.NotContains(t, stripped.AllowedTools, WorkflowToolName,
		"subagent must not have the workflow tool")
	// Original must be unmodified.
	require.Contains(t, agent.AllowedTools, WorkflowToolName,
		"original agent config must not be mutated")
}

// TestStripWorkflowTool_CoderProfile verifies that a coder-profile
// config (which includes all tools including workflow) does not
// retain workflow after stripping.
func TestStripWorkflowTool_CoderProfile(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Options: &config.Options{},
	}
	cfg.SetupAgents()
	coderCfg, ok := cfg.Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must exist")
	require.Contains(t, coderCfg.AllowedTools, WorkflowToolName,
		"coder config should normally include workflow")

	stripped := stripWorkflowTool(coderCfg)
	require.NotContains(t, stripped.AllowedTools, WorkflowToolName,
		"coder subagent must NOT have workflow after stripping")
}

// TestStripWorkflowTool_TaskProfile verifies that the task profile
// (which never has workflow) is unaffected by stripping.
func TestStripWorkflowTool_TaskProfile(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Options: &config.Options{},
	}
	cfg.SetupAgents()
	taskCfg := cfg.Agents[config.AgentTask]
	require.NotContains(t, taskCfg.AllowedTools, WorkflowToolName,
		"task profile should not have workflow to begin with")

	stripped := stripWorkflowTool(taskCfg)
	require.NotContains(t, stripped.AllowedTools, WorkflowToolName)
	require.Equal(t, taskCfg.AllowedTools, stripped.AllowedTools,
		"task profile should be unchanged")
}

func TestCountCoderAgents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"none", `agent("hello")`, 0},
		{"one double-quoted", `agent("do", {agent = "coder"})`, 1},
		{"one single-quoted", `agent("do", {agent = 'coder'})`, 1},
		{"two", `
			parallel({
				{prompt = "a", agent = "coder"},
				{prompt = "b", agent = "coder"},
			})
		`, 2},
		{"mixed", `
			agent("read", {agent = "task"})
			agent("write", {agent = "coder"})
		`, 1},
		{"no-space", `agent("x", {agent="coder"})`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := countCoderAgents(tc.script)
			require.Equal(t, tc.want, got)
		})
	}
}
