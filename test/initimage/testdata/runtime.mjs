// Test double for kind-a2a's bundled execution reporting runtime
// (src/reporting/runtime.ts, `install`): the same local identity, inbox-guard
// and Claude file checks and the same files written in the same order, without
// the remote execution status call or the MCP relay itself.
import { lstatSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const [command, directory, node] = process.argv.slice(2);
if (command !== "install") throw new Error("unsupported runtime command");
const expected = JSON.parse(readFileSync(join(directory, "expected.json"), "utf8"));
if (process.env.AGENT_INSTANCE_ID !== expected.instanceId) throw new Error("runtime identity mismatch");
const control = JSON.parse(readFileSync(join(directory, "inbox-control.json"), "utf8"));
if (control.version !== 1 || control.instance_id !== expected.instanceId ||
    process.env.AGYN_INBOX_JOURNAL_DIR !== "/workspace/.agyn/inbox-journal" ||
    process.env.AGYN_INBOX_CONTROL_FILE !== join(directory, "inbox-control.json")) {
  throw new Error("inbox replay guard is not configured");
}
if (JSON.parse(readFileSync("/agyn/config.json", "utf8")).sdk !== "claude") throw new Error("claude runtime expected");
const settingsFile = join(homedir(), ".claude", "settings.json");
const mcpFile = join(homedir(), ".claude.json");
for (const path of [settingsFile, mcpFile]) {
  if (!lstatSync(path).isFile()) throw new Error("agynd did not prepare the Claude configuration");
}
const user = JSON.parse(readFileSync(mcpFile, "utf8"));
user.mcpServers = { ...(user.mcpServers ?? {}),
  execution_reporting: { type: "stdio", command: node, args: [join(directory, "runtime.mjs"), "mcp", directory] } };
writeFileSync(`${mcpFile}.execution.tmp`, JSON.stringify(user, null, 2) + "\n", { mode: 0o600, flag: "wx" });
renameSync(`${mcpFile}.execution.tmp`, mcpFile);
writeFileSync(join(directory, "configured.tmp"), JSON.stringify({ ...expected, reportingConfigured: true }), { mode: 0o600, flag: "wx" });
renameSync(join(directory, "configured.tmp"), join(directory, "configured.json"));
