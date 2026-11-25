// Entry point of Meko's context engine, which `meko serve` runs as a supervised child.
// mem0 reads its telemetry switch and its config folder when it's first imported, so they're set
// here before anything imports it, as is the umask.
process.env.MEM0_TELEMETRY = "false";
// What the context engine writes in the data folder (the model cache, mem0's config) is the user's only.
process.umask(0o077);
if (process.env.MEKO_CONTEXT_ENGINE_STATE_DIR) process.env.MEM0_DIR = process.env.MEKO_CONTEXT_ENGINE_STATE_DIR;

const { run } = await import("./app.js");
await run(process.env);
