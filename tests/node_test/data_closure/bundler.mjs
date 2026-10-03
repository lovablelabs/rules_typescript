import { createRequire } from "node:module";
import { basename, extname, join, resolve } from "node:path";
import { parseArgs } from "node:util";

const { build } = createRequire(import.meta.url)("esbuild");
const { values } = parseArgs({
  options: {
    entry: { type: "string" },
    "out-dir": { type: "string" },
    format: { type: "string" },
  },
});
await build({
  entryPoints: [resolve(values.entry)],
  outfile: join(resolve(values["out-dir"]), `${basename(values.entry, extname(values.entry))}.js`),
  bundle: true,
  platform: "node",
  format: values.format,
});
