Get stdout/stderr from a background shell by ID.

You are notified automatically when a background job finishes, so this tool is for inspecting a job *before* it completes — checking a server's startup log, or a long build's progress. Do not call it in a polling loop to wait for completion.

wait=true blocks the turn until the shell completes. Prefer leaving it false and continuing with other work: the completion notice will reach you either way.
