import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const [, , input, output, mode] = process.argv;
const names = JSON.parse(readFileSync(input, "utf8"));
const members = names.map((member) => `readonly ${JSON.stringify(member)}: string;`).join(" ");
if (mode === "ambient") {
  mkdirSync(output, { recursive: true });
  writeFileSync(join(output, "authored.d.ts"), "declare const authoredCollision: number;\n");
  if (names.length) {
    writeFileSync(
      join(output, "ambient.d.ts"),
      `declare module "generated-api" { export const api: { ${members} }; }\n`,
    );
  }
} else if (mode === "tree") {
  mkdirSync(output, { recursive: true });
  for (const name of names) {
    writeFileSync(
      join(output, `${name}.d.ts`),
      `import type { z } from "zod";\nexport declare const ${name}: z.infer<z.ZodString>;\nexport declare const generatedTreeMembers: { ${members} };\n`,
    );
  }
} else if (mode === "config-leaf" || mode === "config-base") {
  mkdirSync(dirname(output), { recursive: true });
  const config = mode === "config-leaf"
    ? { extends: "./base.json", files: ["value.ts"] }
    : { compilerOptions: { strict: true, module: "Preserve", moduleResolution: "Bundler" } };
  writeFileSync(output, JSON.stringify(config) + "\n");
} else if (mode === "json") {
  mkdirSync(dirname(output), { recursive: true });
  writeFileSync(output, JSON.stringify({ current: names.join(",") }) + "\n");
} else {
  mkdirSync(dirname(output), { recursive: true });
  writeFileSync(
    output,
    `export const generatedValue: string = ${JSON.stringify(names.join(","))};\nexport const generatedMembers = ${JSON.stringify(Object.fromEntries(names.map((name) => [name, name])))} as const;\ndeclare global { interface GeneratedGlobals { ${members} } }\n`,
  );
}
