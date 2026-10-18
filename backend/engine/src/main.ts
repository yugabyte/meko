// Entry point of Meko Local's memory engine, which `meko-local serve` runs as a supervised child.
// mem0 reads its telemetry switch and its config folder when it's first imported, so they're set
// here before anything imports it.
process.env.MEM0_TELEMETRY = "false";
if (process.env.MEKO_ENGINE_STATE_DIR) process.env.MEM0_DIR = process.env.MEKO_ENGINE_STATE_DIR;

const { run } = await import("./app.js");
await run(process.env);
