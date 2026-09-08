Launch a new agent that has access to the following tools: glob, grep, ls, view. When you are searching for a keyword or file and are not confident that you will find the right match on the first try, use the agent tool to perform the search for you.

## Async behavior

The agent runs in the background. The tool returns immediately with a job ID, and the result arrives later as a `<task-notification>` message.

- Do NOT poll, sleep, or call job_output in a loop waiting for it.
- Never fabricate or predict the result.
- If the user asks before the notification lands, say the agent is still running.
- Use job_kill with the job ID to stop an agent.
