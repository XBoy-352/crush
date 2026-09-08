Terminate a background shell, subagent, or workflow job.

<usage>
- Provide the ID returned from a background bash execution, or from an `agent` / `agentic_fetch` / `workflow` tool call
- Cancels the running process or child job and cleans up resources
</usage>

<features>
- Stop long-running background shells
- Stop a running subagent (`agent-001`) or fetch job (`fetch-002`)
- Stop a whole workflow (`wf-003`) including its workers, or one worker (`wf-w-004`)
- Clean up completed background shells
</features>

<tips>
- Use this when you need to stop a background process or a still-running subagent
- The process is terminated immediately (similar to SIGTERM)
- After killing, the ID becomes invalid
</tips>
