package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/childjobs"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobOutputToolName = "job_output"
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell or subagent/workflow job to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes before returning output"`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command,omitempty"`
	Description      string `json:"description,omitempty"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

func NewJobOutputTool(children *childjobs.Registry) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()
			bgShell, isShell := bgManager.Get(params.ShellID)
			if !isShell {
				if job, isJob := children.Get(params.ShellID); isJob {
					return childJobOutput(params.ShellID, job)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell or job not found: %s", params.ShellID)), nil
			}

			if params.Wait {
				rootSessionID := GetRootSessionFromContext(ctx)
				releaseCh := shell.RegisterForegroundWait(rootSessionID, bgShell.ID)
				defer shell.UnregisterForegroundWait(rootSessionID, bgShell.ID)

				waitDone := make(chan struct{})
				go func() {
					bgShell.WaitContext(ctx)
					close(waitDone)
				}()

				select {
				case <-waitDone:
				case <-releaseCh:
					// User pressed Ctrl+B to release foreground wait
				case <-ctx.Done():
					return fantasy.ToolResponse{}, ctx.Err()
				}
			}

			stdout, stderr, done, err := bgShell.GetOutput()
			if done {
				// The agent has the outcome; a notice would repeat it.
				bgShell.DiscardCompletionNotice()
			}

			var outputParts []string
			if stdout != "" {
				outputParts = append(outputParts, stdout)
			}
			if stderr != "" {
				outputParts = append(outputParts, stderr)
			}

			status := "running"
			if done {
				status = "completed"
				if err != nil {
					exitCode := shell.ExitCode(err)
					if exitCode != 0 {
						outputParts = append(outputParts, fmt.Sprintf("Exit code %d", exitCode))
					}
				}
			}

			output := strings.Join(outputParts, "\n")
			output = TruncateOutput(output)

			metadata := JobOutputResponseMetadata{
				ShellID:          params.ShellID,
				Command:          bgShell.Command,
				Description:      bgShell.Description,
				Done:             done,
				WorkingDirectory: bgShell.WorkingDir,
			}

			if output == "" {
				output = BashNoOutput
			}

			result := fmt.Sprintf("Status: %s\n\n%s", status, output)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}

// childJobOutput reports a subagent/workflow job's status and, when
// terminal, its full result or error.
func childJobOutput(id string, job *childjobs.Job) (fantasy.ToolResponse, error) {
	status, result, jobErr := job.Snapshot()

	switch status {
	case childjobs.StatusDone:
		out := fmt.Sprintf("Status: completed\n\n%s", result)
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(out), JobOutputResponseMetadata{
			ShellID:     id,
			Description: job.Title,
			Done:        true,
		}), nil
	case childjobs.StatusError:
		out := fmt.Sprintf("Status: failed\n\n%s", jobErr)
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(out), JobOutputResponseMetadata{
			ShellID:     id,
			Description: job.Title,
			Done:        true,
		}), nil
	case childjobs.StatusKilled:
		out := fmt.Sprintf("Status: killed\n\n%s", jobErr)
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(out), JobOutputResponseMetadata{
			ShellID:     id,
			Description: job.Title,
			Done:        true,
		}), nil
	default:
		verb := "running"
		if status == childjobs.StatusQueued {
			verb = "queued"
		}
		out := fmt.Sprintf("Job %s is still %s (started %s ago). You will be notified when it completes.", id, verb, childJobElapsed(job.StartedAt))
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(out), JobOutputResponseMetadata{
			ShellID:     id,
			Description: job.Title,
			Done:        false,
		}), nil
	}
}
