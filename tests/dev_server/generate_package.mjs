import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

const output = process.argv[2];
for (const [file, content] of Object.entries({
  'package.json': JSON.stringify({ type: 'module', main: './dist/client.js' }),
  'index.js': 'export const marker = "WRONG_GENERATED_INDEX";\n',
  'dist/client.js': 'export const marker = "DECLARED_PACKAGE_MAIN";\n',
  'dist/extension.mjs': 'export const marker = "DECLARED_EXTENSIONLESS_MJS";\n',
  'escape/package.json': JSON.stringify({ type: 'module', main: '../../outside.js' }),
})) {
  const destination = join(output, file);
  mkdirSync(dirname(destination), { recursive: true });
  writeFileSync(destination, content);
}
