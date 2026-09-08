package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/childjobs"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobKillToolName = "job_kill"
)

//go:embed job_kill.md
var jobKillDescription string

type JobKillParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell or subagent/workflow job to terminate"`
}

type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
}

func NewJobKillTool(children *childjobs.Registry) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobKillToolName,
		jobKillDescription,
		func(ctx context.Context, params JobKillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()

			bgShell, ok := bgManager.Get(params.ShellID)
			if ok {
				metadata := JobKillResponseMetadata{
					ShellID:     params.ShellID,
					Command:     bgShell.Command,
					Description: bgShell.Description,
				}

				err := bgManager.Kill(params.ShellID)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}

				result := fmt.Sprintf("Background shell %s terminated successfully", params.ShellID)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
			}

			job, ok := children.Get(params.ShellID)
			if ok {
				err := children.Kill(params.ShellID)
				if err != nil {
					if errors.Is(err, childjobs.ErrNotFound) {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("job not found or already finished: %s", params.ShellID)), nil
					}
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				kind := "subagent job"
				if job.Kind == childjobs.KindWorkflow {
					kind = "workflow job (and its workers)"
				}
				result := fmt.Sprintf("%s %s terminated", kind, params.ShellID)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), JobKillResponseMetadata{
					ShellID:     params.ShellID,
					Description: job.Title,
				}), nil
			}

			return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell or job not found: %s", params.ShellID)), nil
		},
	)
}

// childJobElapsed formats a human-friendly elapsed time for a child job.
func childJobElapsed(started time.Time) string {
	d := time.Since(started).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return d.Truncate(time.Minute).String()
}
