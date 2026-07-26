package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/workflow"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
)

//go:embed templates/workflow_tool.md
var workflowToolDescription string

// WorkflowToolName is the name of the workflow tool.
const WorkflowToolName = "workflow"

// WorkflowParams is the parameters for the workflow tool.
type WorkflowParams struct {
	Description string `json:"description" description:"One-line summary of what this workflow does; shown in the permission prompt"`
	Script      string `json:"script" description:"Lua orchestration script; see tool description for the API"`
}

// workflowAgents holds pre-built agents for each profile so that
// spawn can pick the right one without re-creating them per call.
type workflowAgents struct {
	task  SessionAgent
	coder SessionAgent
}

// stripWorkflowTool returns a copy of the agent config with the
// "workflow" tool removed from AllowedTools. This prevents recursive
// fan-out: a subagent must never invoke the workflow tool itself.
func stripWorkflowTool(agent config.Agent) config.Agent {
	agent.AllowedTools = slices.DeleteFunc(
		slices.Clone(agent.AllowedTools),
		func(s string) bool { return s == WorkflowToolName },
	)
	return agent
}

func (c *coordinator) workflowTool(ctx context.Context) (fantasy.AgentTool, error) {
	// Build agents for both profiles, stripping the workflow tool
	// from each to prevent recursive fan-out (safety 3a).
	taskCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent configuration not found")
	}
	taskCfg = stripWorkflowTool(taskCfg)

	coderCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errors.New("coder agent configuration not found")
	}
	coderCfg = stripWorkflowTool(coderCfg)

	taskPrmpt, err := taskPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build task prompt: %w", err)
	}

	coderPrmpt, err := coderPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build coder prompt: %w", err)
	}

	taskAgent, err := c.buildAgent(ctx, taskPrmpt, taskCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build task agent: %w", err)
	}

	coderAgent, err := c.buildAgent(ctx, coderPrmpt, coderCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build coder agent: %w", err)
	}

	agents := workflowAgents{
		task:  taskAgent,
		coder: coderAgent,
	}

	return fantasy.NewAgentTool(
		WorkflowToolName,
		workflowToolDescription,
		func(ctx context.Context, params WorkflowParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Description == "" {
				return fantasy.NewTextErrorResponse("Description cannot be empty"), nil
			}
			if params.Script == "" {
				return fantasy.NewTextErrorResponse("Script cannot be empty"), nil
			}
			if len(params.Script) > workflow.MaxScriptBytes {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Script length exceeds maximum of %d bytes", workflow.MaxScriptBytes)), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session context missing")
			}
			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("message context missing")
			}

			// Safety 3c: count coder-profile agents in the script
			// so the permission prompt can state what write
			// capabilities are requested.
			coderCount := countCoderAgents(params.Script)
			desc := params.Description
			if coderCount > 0 {
				desc = fmt.Sprintf("%s [%d write-capable (coder) agent(s) requested]", desc, coderCount)
			}

			p, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        c.cfg.WorkingDir(),
				ToolCallID:  call.ID,
				ToolName:    WorkflowToolName,
				Action:      "execute",
				Description: desc,
				Params:      params,
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			spawn := func(ctx context.Context, index int, label, prompt string, opts workflow.SpawnOpts) (string, error) {
				title := label
				if title == "" {
					title = fmt.Sprintf("Workflow Agent %d", index+1)
				}

				// Select the agent based on the requested profile.
				// Empty defaults to "task" to preserve existing behaviour.
				agent := agents.task
				if opts.Agent == "coder" {
					agent = agents.coder
				}

				subParams := subAgentParams{
					Agent:          agent,
					SessionID:      sessionID,
					AgentMessageID: agentMessageID,
					ToolCallID:     fmt.Sprintf("%s-a%d", call.ID, index),
					Prompt:         prompt,
					SessionTitle:   title,
				}

				// Coder-profile subagents auto-approve their
				// session so that writes don't prompt individually
				// (the single workflow permission covers them).
				// Task-profile subagents are read-only and don't
				// need auto-approval.
				if opts.Agent == "coder" {
					subParams.SessionSetup = func(id string) {
						c.permissions.AutoApproveSession(id)
					}
				}

				resp, err := c.runSubAgent(ctx, subParams)
				if err != nil {
					return "", err
				}
				if resp.IsError {
					return "", errors.New(resp.Content)
				}
				return resp.Content, nil
			}

			result, err := workflow.Run(ctx, params.Script, spawn, workflow.Options{})
			if err != nil {
				if ctx.Err() != nil {
					return fantasy.ToolResponse{}, err
				}

				logsStr := "(none)"
				if len(result.Logs) > 0 {
					var sb strings.Builder
					for _, l := range result.Logs {
						sb.WriteString("- ")
						sb.WriteString(l)
						sb.WriteString("\n")
					}
					logsStr = sb.String()
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextErrorResponse(fmt.Sprintf("workflow failed: %v\n\nLogs:\n%s", err, logsStr)),
					map[string]any{"agents": result.AgentCount, "logs": result.Logs},
				), nil
			}

			var valStr string
			if result.Value == "" {
				valStr = "null"
			} else {
				valStr = result.Value
			}

			logsStr := "(none)"
			if len(result.Logs) > 0 {
				var sb strings.Builder
				for _, l := range result.Logs {
					sb.WriteString("- ")
					sb.WriteString(l)
					sb.WriteString("\n")
				}
				logsStr = sb.String()
			}

			out := fmt.Sprintf("Workflow finished: %d agent(s) run.\n\nReturn value:\n```json\n%s\n```\n\nLogs:\n%s", result.AgentCount, valStr, logsStr)

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(out),
				map[string]any{"agents": result.AgentCount, "logs": result.Logs},
			), nil
		},
	), nil
}

// countCoderAgents does a best-effort count of coder-profile agents
// in a Lua script by looking for agent = "coder" patterns. This is
// used to enrich the permission prompt (safety 3c).
func countCoderAgents(script string) int {
	count := 0
	for _, pattern := range []string{
		`agent = "coder"`,
		`agent = 'coder'`,
		`agent="coder"`,
		`agent='coder'`,
	} {
		count += strings.Count(script, pattern)
	}
	return count
}
