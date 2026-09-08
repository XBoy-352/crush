Fetch a URL or search the web using an AI sub-agent that can extract, summarize, and answer questions. Slower and costlier than fetch; use fetch for raw content or API responses.


## Async behavior

The research sub-agent runs in the background. The tool returns immediately with a job ID, and the result arrives later as a `<task-notification>` message. Do NOT poll, sleep, or call job_output in a loop waiting for it, and never fabricate the result. Use job_kill with the job ID to stop it.
