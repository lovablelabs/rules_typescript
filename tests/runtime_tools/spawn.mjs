import { execFileSync } from "node:child_process";
import { isAbsolute } from "node:path";

const tool = process.env.RT_PROBE_TOOL;
if (!tool || !isAbsolute(tool)) {
  throw new Error(`RT_PROBE_TOOL is not an absolute path: ${tool}`);
}
const out = execFileSync(tool, ["from", "ts_binary"], { encoding: "utf8" });
if (out.trim() !== "probe ran with from ts_binary") {
  throw new Error(`unexpected probe output: ${out}`);
}
console.log(out.trim());
