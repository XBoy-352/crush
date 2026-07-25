package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
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

func (c *coordinator) workflowTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent configuration not found")
	}

	prmpt, err := taskPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build task prompt: %w", err)
	}

	agent, err := c.buildAgent(ctx, prmpt, agentCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build task agent: %w", err)
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

			p, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        c.cfg.WorkingDir(),
				ToolCallID:  call.ID,
				ToolName:    WorkflowToolName,
				Action:      "execute",
				Description: params.Description,
				Params:      params,
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			spawn := func(ctx context.Context, index int, label, prompt string) (string, error) {
				title := label
				if title == "" {
					title = fmt.Sprintf("Workflow Agent %d", index+1)
				}
				resp, err := c.runSubAgent(ctx, subAgentParams{
					Agent:          agent,
					SessionID:      sessionID,
					AgentMessageID: agentMessageID,
					ToolCallID:     fmt.Sprintf("%s-a%d", call.ID, index),
					Prompt:         prompt,
					SessionTitle:   title,
					SessionSetup:   func(id string) { c.permissions.AutoApproveSession(id) },
				})
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
