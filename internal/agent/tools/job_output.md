Get output from a background shell or a subagent/workflow job by ID.

You are notified automatically when a background job finishes, so this tool is for inspecting a job *before* it completes — checking a server's startup log, a long build's progress, or a still-running subagent. Do not call it in a polling loop to wait for completion.

wait=true blocks the turn until a *shell* completes. Prefer leaving it false and continuing with other work: the completion notice will reach you either way. wait=true has no effect on subagent jobs; they report current status immediately.
