// A real tsserver process over its own protocol: tsserver resolves through its
// LanguageServiceHost, and a hook on ts.resolveModuleName is invisible to it.

import { spawnSync } from 'node:child_process';
import {
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { dirname, join } from 'node:path';
import { startServer as startTsserver } from './tsserver_client.mjs';

const [, , tsserverJs, workspaceRoot, refresh, generatedEntrypointConfig] = process.argv;
const timeoutSeconds = Number(process.env.TEST_TIMEOUT ?? 0);
if (!Number.isSafeInteger(timeoutSeconds) || timeoutSeconds < 0) {
  throw new Error(`invalid enclosing TEST_TIMEOUT: ${process.env.TEST_TIMEOUT}`);
}
const startedAt = Date.now();
const deadline = timeoutSeconds ? startedAt + timeoutSeconds * 1000 : 0;

if (!tsserverJs || !workspaceRoot || !refresh || !generatedEntrypointConfig) {
  process.stderr.write(
    'FATAL: usage: tsserver_plugin_test.mjs <tsserver.js> <workspace_root> <refresh> <generated_entrypoint_config>\n'
  );
  process.exit(2);
}

const PLUGIN_NAME = '@rules_typescript/tsserver-plugin';
const PLUGIN_DIR = join(workspaceRoot, '.bazel/node_modules', PLUGIN_NAME);
const GOOD = join(workspaceRoot, 'fixture/src/good.ts');
const BAD = join(workspaceRoot, 'fixture/src/bad.ts');
const BOGUS_MEMBER = 'definitelyNotAMethod';

// The first-party package the fixture imports: the first one the staged hook
// data names, so the test follows the graph rather than pinning a label.
const hookContents = readFileSync(join(workspaceRoot, '.bazel/tsserver-hook-data.json'), 'utf8');
const hookData = JSON.parse(hookContents);
const PKG = (hookData.packages || [])[0];
if (!PKG) {
  process.stderr.write('FATAL: the staged hook data names no package\n');
  process.exit(1);
}
const LIB_DECLARATIONS = join('bazel-bin', PKG);
const rootProject = join(workspaceRoot, 'tsconfig.json');
const rootConfig = JSON.parse(readFileSync(rootProject, 'utf8'));
const ordinaryPackage = hookData.packages.find((pkg) =>
  Object.hasOwn(rootConfig.compilerOptions.paths, pkg)
);
if (!ordinaryPackage) {
  throw new Error('ordinary refresh did not install an exact first-party package alias');
}
const ordinaryConsumer = join(workspaceRoot, 'ordinary.ts');
const ordinarySubpathConsumer = join(workspaceRoot, 'ordinary-subpath.ts');
const ordinaryUse = 'export const value: number = lib.';
const ordinaryMembers = [
  'declarationOnly',
  'generatedOnly',
  'sourceOnly',
  'addedLater',
  'replacementOnly',
  'restoredOnly',
  'staleOnly',
];
const ordinaryDeclaration =
  'export declare function add(a: number, b: number): number;\n' +
  'export declare const declarationOnly: true;\n';

function write(rel, contents) {
  const p = join(workspaceRoot, rel);
  mkdirSync(dirname(p), { recursive: true });
  writeFileSync(p, contents);
}

// What a build leaves for the worker: the .d.ts in bazel-bin wins over a source.
write(
  `${LIB_DECLARATIONS}/index.d.ts`,
  'export declare function add(a: number, b: number): number;\n'
);

// The fixture's own tsconfig, with no `paths`: tsserver builds these files'
// program from it, so the package is reachable only through the plugin it names.
write(
  'fixture/tsconfig.json',
  JSON.stringify(
    {
      compilerOptions: {
        target: 'ES2022',
        module: 'Preserve',
        moduleResolution: 'Bundler',
        strict: true,
        noEmit: true,
        skipLibCheck: true,
        plugins: [{ name: PLUGIN_NAME }],
      },
      include: ['src'],
    },
    null,
    2
  ) + '\n'
);
write(
  'fixture/src/good.ts',
  `import * as lib from "${PKG}";\nexport const s: number = lib.add(1, 2);\n`
);
write(
  'fixture/src/bad.ts',
  `import * as lib from "${PKG}";\nexport const s = lib.${BOGUS_MEMBER}();\n`
);
function consumerSource(specifier) {
  return (
    `import * as lib from "${specifier}";\n${ordinaryUse}add(1, 2);\n` +
    `export const broken = lib.${BOGUS_MEMBER}();\n`
  );
}
write('ordinary.ts', consumerSource(ordinaryPackage));
write('ordinary-subpath.ts', consumerSource(`${ordinaryPackage}/index`));
write(`bazel-bin/${ordinaryPackage}/index.d.ts`, ordinaryDeclaration);
write(
  `${ordinaryPackage}/index.ts`,
  'export function add(a: number, b: number): number { return a + b; }\n' +
    'export const sourceOnly = true;\n'
);
const generatedConfig = readFileSync(generatedEntrypointConfig, 'utf8');
const generatedPackage = Object.keys(JSON.parse(generatedConfig).compilerOptions.paths).find(
  (key) => !key.includes('*')
);
if (!generatedPackage) throw new Error('generated entrypoint fixture has no package alias');
const generatedDirectory = 'generated-only';
const generatedConsumer = join(workspaceRoot, generatedDirectory, 'consumer.ts');
const generatedOutput = `${generatedDirectory}/bazel-bin/${generatedPackage}/index`;
write(`${generatedDirectory}/tsconfig.json`, generatedConfig);
write(`${generatedDirectory}/consumer.ts`, consumerSource(generatedPackage));
write(`${generatedOutput}.d.ts`, ordinaryDeclaration);
write(
  `${generatedDirectory}/${generatedPackage}/index.ts`,
  'export function add(a: number, b: number): boolean { return true; }\n' +
    'export const staleOnly = true;\n'
);

const GRAPH = join(workspaceRoot, 'fixture/src/graph.ts');
const graphPackages = ['first', 'second'].map((name, index) => {
  const pkg = `fixture/graph/${name}`;
  const member = `${name}Value`;
  write(`${pkg}/index.ts`, `export const ${member} = ${index};\n`);
  return {
    name,
    pkg,
    member,
    line: index + 3,
    offset: `export const ${name}Result = ${name}.`.length + 1,
  };
});
write(
  'fixture/src/graph.ts',
  graphPackages.map(({ name, pkg }) => `import * as ${name} from "${pkg}";\n`).join('') +
    graphPackages
      .map(({ name, member }) => `export const ${name}Result = ${name}.${member};\n`)
      .join('')
);

if (!Object.hasOwn(rootConfig.compilerOptions.paths, `${ordinaryPackage}/*`)) {
  throw new Error('ordinary refresh did not install the ancestor wildcard');
}
const fragmentPackages = ['first', 'second'].map((name, index) => {
  const pkg = `${ordinaryPackage}/fragment-${name}`;
  const member = `${name}Value`;
  write(`${pkg}/BUILD.bazel`, 'package(default_visibility = ["//visibility:private"])\n');
  write(`${pkg}/index.ts`, `export function ${member}(): boolean { return true; }\n`);
  write(
    `${pkg}.ts`,
    `export function ${member}(): string { return "stale"; }\nexport const staleOnly = true;\n`
  );
  write(
    `bazel-bin/${pkg}/index.d.ts`,
    `export declare function ${member}(): number;\nexport declare const generatedOnly: true;\n`
  );
  return { name, pkg, member, line: index + 3 };
});
write(`${ordinaryPackage}/BUILD.bazel`, '');
const fragmentDestination = `bazel-bin/${ordinaryPackage}/private.tsconfig-fragment.json`;
const fragmentSource =
  fragmentPackages.map(({ name, pkg }) => `import * as ${name} from "${pkg}";\n`).join('') +
  fragmentPackages
    .map(({ name, member }) => `export const ${name}Result: number = ${name}.${member}();\n`)
    .join('');
const fragmentConsumers = [
  { file: join(workspaceRoot, 'fragment-graph.ts'), project: rootProject },
  {
    file: join(workspaceRoot, 'fragment-inherited/consumer.ts'),
    project: join(workspaceRoot, 'fragment-inherited/tsconfig.json'),
  },
];
write('fragment-graph.ts', fragmentSource);
write('fragment-inherited/consumer.ts', fragmentSource);
write(
  'fragment-inherited/tsconfig.json',
  JSON.stringify({ extends: '../tsconfig.json', files: ['consumer.ts'], include: [] })
);
write(
  'fragment-native.ts',
  'export function firstValue(): bigint { return 1n; }\nexport const authoredOnly = true;\n'
);
write(
  'fragment-authored.json',
  JSON.stringify({
    extends: './tsconfig.json',
    compilerOptions: {
      paths: {
        [fragmentPackages[0].pkg]: ['./fragment-native.ts'],
        [`${ordinaryPackage}/*`]: ['./missing/*'],
      },
    },
  })
);
write(
  'fragment-authored/tsconfig.json',
  JSON.stringify({ extends: '../fragment-authored.json', files: ['consumer.ts'], include: [] })
);
write('fragment-authored/consumer.ts', fragmentSource);
const authoredFragmentConsumer = join(workspaceRoot, 'fragment-authored/consumer.ts');

const publicationRoot = join(dirname(workspaceRoot), 'graph-publication');
mkdirSync(publicationRoot, { recursive: true });
const graphArtifact = join(publicationRoot, 'graph.json');
const publicationManifest = join(publicationRoot, 'copy.json');
const runfilesManifest = join(publicationRoot, 'runfiles_manifest');
writeFileSync(runfilesManifest, `manifest ${publicationManifest}\ngraph ${graphArtifact}\n`);

function publishGraph(contents, destination = '.bazel/tsserver-hook-data.json', phase) {
  if (phase)
    process.stdout.write(`fragment ${phase} publication: started at ${Date.now() - startedAt}ms\n`);
  writeFileSync(publicationManifest, JSON.stringify([{ rlocation: 'graph', dest: destination }]));
  writeFileSync(graphArtifact, contents);
  const result = spawnSync(refresh, [], {
    cwd: workspaceRoot,
    encoding: 'utf8',
    ...(deadline ? { timeout: Math.max(1, deadline - Date.now()) } : {}),
    env: {
      ...process.env,
      BUILD_WORKSPACE_DIRECTORY: workspaceRoot,
      COPY_TO_WORKSPACE_MANIFEST: 'manifest',
      RUNFILES_MANIFEST_FILE: runfilesManifest,
      RUNFILES_DIR: '',
    },
  });
  if (result.error || result.status !== 0) {
    throw new Error(`graph publication failed: ${result.error || result.stderr}`);
  }
  if (phase)
    process.stdout.write(
      `fragment ${phase} publication: completed at ${Date.now() - startedAt}ms\n`
    );
}

let failures = 0;

function pass(name) {
  process.stdout.write(`PASS: ${name}\n`);
}

function fail(name, detail) {
  process.stderr.write(`FAIL: ${name}${detail ? ': ' + detail : ''}\n`);
  failures += 1;
}

const describe = (diagnostics) =>
  diagnostics.length === 0
    ? '(clean)'
    : JSON.stringify(diagnostics.map((d) => `TS${d.code} ${d.text}`));

function startServer({ plugin, env }) {
  return startTsserver({ tsserverJs, workspaceRoot, plugin, deadline, env });
}

// The worker's first map can arrive after the initial unresolved answer.
async function settle(observe, accept) {
  let last = await observe();
  while (!(await accept(last)) && (!deadline || Date.now() < deadline)) {
    await new Promise((r) => setTimeout(r, 250));
    last = await observe();
  }
  return last;
}

const missesPackage = (diagnostics) =>
  diagnostics.some((d) => d.code === 2307 && d.text.includes(`'${PKG}'`));

async function checkOrdinaryState(server, file, selected, member, type, lane) {
  const expected = selected && realpathSync(join(workspaceRoot, selected));
  const position = { file, line: 2, offset: ordinaryUse.length + 1 };
  let completions;
  let definitions;
  let hover;
  let matches;
  const diagnostics = await settle(() => server.diagnostics(file), async (diagnostics) => {
    completions = (await server.request('completionInfo', position))?.entries || [];
    const inside = { ...position, offset: position.offset + 1 };
    definitions = (await server.request('definition', inside)) || [];
    if (!selected) {
      matches =
        diagnostics.length === 1 &&
        diagnostics[0].code === 2307 &&
        ordinaryMembers.every((name) => !completions.some((entry) => entry.name === name)) &&
        definitions.length === 0;
      return matches;
    }
    hover = await server.request('quickinfo', inside);
    matches =
      diagnostics.length === (type === 'number' ? 1 : 2) &&
      diagnostics.some(
        (diagnostic) => diagnostic.code === 2339 && diagnostic.text.includes(BOGUS_MEMBER)
      ) &&
      (type === 'number' ||
        diagnostics.some(
          (diagnostic) =>
            diagnostic.code === 2322 &&
            diagnostic.text.includes(`Type '${type}' is not assignable to type 'number'`)
        )) &&
      hover?.displayString.includes(`): ${type}`) &&
      ordinaryMembers.every(
        (name) => completions.some((entry) => entry.name === name) === (name === member)
      ) &&
      definitions.length > 0 &&
      definitions.every((entry) => existsSync(entry.file) && realpathSync(entry.file) === expected);
    return matches;
  });
  if (matches) {
    pass(`ordinary ${lane}: ${file} observes ${selected || 'deleted generated entrypoint'}`);
  } else {
    fail(
      `ordinary ${lane}: ${file} must observe ${selected || 'deleted generated entrypoint'}`,
      JSON.stringify({ diagnostics, completions, definitions, hover })
    );
  }
}

async function checkGraphState(server, active, name) {
  const before = failures;
  const misses = (diagnostics, pkg) =>
    diagnostics.some((d) => d.code === 2307 && d.text.includes(`'${pkg}'`));
  const diagnostics = await settle(() => server.diagnostics(GRAPH), (d) =>
    graphPackages.every(({ pkg }) => misses(d, pkg) === !active.includes(pkg))
  );
  for (const { pkg, member, line, offset } of graphPackages) {
    const present = active.includes(pkg);
    if (misses(diagnostics, pkg) === present) {
      fail(`${name}: ${pkg} resolution`, describe(diagnostics));
    }
    const position = { file: GRAPH, line, offset };
    const completion = await server.request('completionInfo', position);
    if ((completion?.entries || []).some((entry) => entry.name === member) !== present) {
      fail(`${name}: ${pkg} completion`, `expected ${member} present=${present}`);
    }
    const definitions =
      (await server.request('definition', { ...position, offset: offset + 1 })) || [];
    const expected = realpathSync(join(workspaceRoot, pkg, 'index.ts'));
    const definitionMatches = present
      ? definitions.some((entry) => realpathSync(entry.file) === expected)
      : definitions.length === 0;
    if (!definitionMatches) {
      fail(`${name}: ${pkg} definition`, JSON.stringify(definitions));
    }
  }
  if (failures === before) pass(name);
}

async function checkFragmentState(server, active, phase) {
  let consumer;
  let previous;
  function observe(predicates) {
    const current = JSON.stringify(predicates);
    if (current !== previous)
      process.stdout.write(
        `fragment ${phase} ${consumer} predicates at ${Date.now() - startedAt}ms: ${current}\n`
      );
    previous = current;
  }
  for (const { file, project } of fragmentConsumers) {
    consumer = project === rootProject ? 'root' : 'inherited';
    previous = undefined;
    const info = await server.request('projectInfo', { file, needFileNameList: false });
    if (realpathSync(info.configFileName) !== realpathSync(project)) {
      throw new Error(`fragment consumer selected the wrong project: ${JSON.stringify(info)}`);
    }
    let matches;
    let observations;
    const diagnostics = await settle(() => server.diagnostics(file), async (diagnostics) => {
      observations = [];
      matches =
        diagnostics.length === fragmentPackages.length - active.length &&
        diagnostics.every((diagnostic) => diagnostic.code === 2322);
      const predicates = {
        diagnostics: matches,
        diagnosticCodes: diagnostics.map(({ code }) => code),
        members: [],
      };
      for (const { name, pkg, member, line } of fragmentPackages) {
        const generated = active.includes(pkg);
        const expected = realpathSync(
          join(workspaceRoot, generated ? `bazel-bin/${pkg}/index.d.ts` : `${pkg}.ts`)
        );
        const position = {
          file,
          line,
          offset: `export const ${name}Result: number = ${name}.`.length + 1,
        };
        const completions = (await server.request('completionInfo', position))?.entries || [];
        const inside = { ...position, offset: position.offset + 1 };
        const definitions = (await server.request('definition', inside)) || [];
        const hover = await server.request('quickinfo', inside);
        const memberPredicates = {
          member: completions.some((entry) => entry.name === member),
          generated: completions.some((entry) => entry.name === 'generatedOnly') === generated,
          stale: completions.some((entry) => entry.name === 'staleOnly') === !generated,
          definition:
            definitions.length > 0 &&
            definitions.every((entry) => realpathSync(entry.file) === expected),
          hover: Boolean(hover?.displayString.includes(`(): ${generated ? 'number' : 'string'}`)),
        };
        matches &&= Object.values(memberPredicates).every(Boolean);
        predicates.members.push({ name, ...memberPredicates });
        observations.push({ pkg, completions, definitions, hover });
      }
      observe(predicates);
      return matches;
    });
    if (matches) pass(`fragment changes preserve package identity through ${project}`);
    else {
      fail(
        `fragment changes must preserve package identity through ${project}`,
        JSON.stringify({ diagnostics, observations })
      );
    }
  }

  const file = authoredFragmentConsumer;
  consumer = 'authored';
  previous = undefined;
  const position = {
    file,
    line: 3,
    offset: 'export const firstResult: number = first.'.length + 1,
  };
  let observations;
  let matches;
  const diagnostics = await settle(() => server.diagnostics(file), async (diagnostics) => {
    const completions = (await server.request('completionInfo', position))?.entries || [];
    const inside = { ...position, offset: position.offset + 1 };
    const definitions = (await server.request('definition', inside)) || [];
    const hover = await server.request('quickinfo', inside);
    const predicates = {
      diagnosticCount: diagnostics.length === 2,
      authoredType: diagnostics.some(
        (diagnostic) => diagnostic.code === 2322 && diagnostic.text.includes("Type 'bigint'")
      ),
      missingModule: diagnostics.some(
        (diagnostic) =>
          diagnostic.code === 2307 && diagnostic.text.includes(fragmentPackages[1].pkg)
      ),
      authored: completions.some((entry) => entry.name === 'authoredOnly'),
      generatedOrStale: !completions.some(
        (entry) => entry.name === 'generatedOnly' || entry.name === 'staleOnly'
      ),
      definition:
        definitions.length > 0 &&
        definitions.every(
          (entry) =>
            realpathSync(entry.file) === realpathSync(join(workspaceRoot, 'fragment-native.ts'))
        ),
      hover: Boolean(hover?.displayString.includes('(): bigint')),
    };
    matches = Object.values(predicates).every(Boolean);
    observe({ ...predicates, diagnosticCodes: diagnostics.map(({ code }) => code) });
    observations = { completions, definitions, hover };
    return matches;
  });
  if (matches) {
    pass('same-directory authored paths retain native success and failed wildcard resolution');
  } else {
    fail(
      'same-directory authored paths must remain authoritative over fragments',
      JSON.stringify({ diagnostics, observations })
    );
  }
}

async function disposedProjectsReleaseWorker() {
  const pkg = 'fixture/disposal-lib';
  const fragment = `bazel-bin/${pkg}/disposal.tsconfig-fragment.json`;
  const header = JSON.stringify({
    format: 'tsconfig-fragment-v1',
    label: '//fixture/disposal-lib:lib',
  });
  const contents = `${header}\n${JSON.stringify({ package: pkg, index: true })}\n`;
  write(`${pkg}/BUILD.bazel`, '');
  write(`bazel-bin/${pkg}/index.d.ts`, 'export declare const value: number;\n');
  write(fragment, contents);
  const projects = ['first', 'second'].map((name) => {
    const file = join(workspaceRoot, `fixture/disposal-${name}.ts`);
    write(
      `fixture/disposal-${name}.ts`,
      `import {value} from '${pkg}';\nexport const result: number = value;\n`
    );
    return { file, projectFileName: join(workspaceRoot, `disposal-${name}.project`) };
  });
  const journal = join(publicationRoot, 'workers.jsonl');
  const bootstrap = join(publicationRoot, 'worker-lifetime.cjs');
  writeFileSync(journal, '');
  writeFileSync(bootstrap, `
    const { appendFileSync } = require('node:fs');
    const threads = require('node:worker_threads');
    const NativeWorker = threads.Worker;
    threads.Worker = class extends NativeWorker {
      constructor(filename, options) {
        super(filename, options);
        if (filename !== ${JSON.stringify(join(PLUGIN_DIR, 'tsserver-hook-worker.js'))}) return;
        const id = this.threadId;
        const record = (event) => appendFileSync(${JSON.stringify(journal)}, JSON.stringify({event, id}) + '\\n');
        record('start');
        this.on('exit', () => record('exit'));
      }
    };
  `);
  const events = () =>
    readFileSync(journal, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
  const server = startServer({
    plugin: 'global',
    env: { NODE_OPTIONS: `--require ${JSON.stringify(bootstrap)}` },
  });
  const open = ({ file, projectFileName }) =>
    server.request('openExternalProject', {
      projectFileName,
      rootFiles: [{ fileName: file }],
      options: {
        module: 'Preserve',
        moduleResolution: 'Bundler',
        strict: true,
        noEmit: true,
        skipLibCheck: true,
      },
      typeAcquisition: { enable: false },
    });
  const diagnostics = ({ file, projectFileName }) =>
    server.request('semanticDiagnosticsSync', { file, projectFileName });
  try {
    for (const project of projects) {
      await open(project);
      const result = await settle(() => diagnostics(project), (d) => d.length === 0);
      if (result.length) {
        throw new Error(`external project did not resolve through the plugin: ${describe(result)}`);
      }
    }
    const starts = events().filter(({ event }) => event === 'start');
    if (starts.length !== 1) {
      throw new Error(`external projects did not share one worker: ${JSON.stringify(events())}`);
    }
    const id = starts[0].id;
    pass('lifecycle: external projects share one workspace worker');

    await server.request('closeExternalProject', { projectFileName: projects[0].projectFileName });
    publishGraph(`${header}\n`, fragment);
    const missing = await settle(() => diagnostics(projects[1]), (d) => d.some(({ code }) => code === 2307));
    if (!missing.some(({ code }) => code === 2307) || events().some(({ event }) => event === 'exit')) {
      throw new Error(
        'disposing one project stopped the shared worker or lost its later fragment publication'
      );
    }
    publishGraph(contents, fragment);
    const restored = await settle(() => diagnostics(projects[1]), (d) => d.length === 0);
    if (restored.length) {
      throw new Error(`surviving project did not observe restored fragment: ${describe(restored)}`);
    }
    pass('lifecycle: disposing one project preserves later map publication for the other');

    await server.request('closeExternalProject', { projectFileName: projects[1].projectFileName });
    await server.request('status', {});
    process.stdout.write('lifecycle: last external project disposed with tsserver still alive\n');
    const exited = () => events().some((event) => event.event === 'exit' && event.id === id);
    await settle(() => server.request('status', {}), exited);
    if (!exited() || !server.alive()) {
      fail(
        'lifecycle: disposing the last project stops its worker while tsserver stays alive',
        JSON.stringify(events())
      );
      return;
    }
    pass('lifecycle: disposing the last project stops its worker while tsserver stays alive');

    await open(projects[0]);
    const reopened = await settle(() => diagnostics(projects[0]), (d) => d.length === 0);
    const replacements = events().filter(({ event }) => event === 'start');
    if (reopened.length || replacements.length !== 2 || replacements[1].id === id) {
      throw new Error(
        `reopened project did not get a fresh working worker: ${describe(reopened)} ${JSON.stringify(events())}`
      );
    }
    pass('lifecycle: reopening a disposed workspace starts a fresh worker and resolves its map');
    await server.request('closeExternalProject', { projectFileName: projects[0].projectFileName });
  } finally {
    await server.stop();
    rmSync(join(workspaceRoot, fragment), { force: true });
  }
}

async function main() {
  for (const file of [
    'index.js',
    'package.json',
    'tsserver-hook-resolver.js',
    'tsserver-hook-worker.js',
  ]) {
    if (!existsSync(join(PLUGIN_DIR, file))) {
      fail(
        'installed: refresh_tsconfig installs the plugin package',
        `${join(PLUGIN_DIR, file)} is missing -- does ts_refresh_tsconfig still ` +
          'copy it to .bazel/node_modules/@rules_typescript/tsserver-plugin?'
      );
      process.exit(1);
    }
  }
  pass('installed: refresh_tsconfig installs the plugin package');

  {
    const child = spawnSync(
      process.execPath,
      [
        '-e',
        `
        const [resolver, workerPath, workspaceRoot, pkg] = process.argv.slice(1);
        const { MessageChannel } = require('node:worker_threads');
        const { port1, port2 } = new MessageChannel();
        // This pending query owns liveness until the background resolver answers.
        port1.on('message', () => {});
        const { createResolutionSource } = require(resolver);
        const source = createResolutionSource({
          workspaceRoot, workerPath,
          onUpdate() {
            if (source.resolve(pkg)) process.stdout.write('resolved\\n');
            else process.exitCode = 1;
            port1.close();
            port2.close();
          },
        });
      `,
        join(PLUGIN_DIR, 'tsserver-hook-resolver.js'),
        join(PLUGIN_DIR, 'tsserver-hook-worker.js'),
        workspaceRoot,
        PKG,
      ],
      {
        encoding: 'utf8',
        ...(deadline ? { timeout: Math.max(1, deadline - Date.now()) } : {}),
      }
    );
    if (child.error || child.status !== 0 || child.stdout !== 'resolved\n') {
      fail(
        'resolver: background worker does not keep the caller alive after its pending query',
        String(child.error || child.stderr || `exit ${child.status}: ${child.stdout}`)
      );
    } else {
      pass('resolver: background worker does not keep the caller alive after its pending query');
    }
  }

  {
    const server = startServer({ plugin: false });
    try {
      server.open(GOOD);
      const diagnostics = await server.diagnostics(GOOD);
      if (missesPackage(diagnostics)) {
        pass(`baseline: tsserver without the plugin cannot find "${PKG}"`);
      } else {
        fail(
          `baseline: tsserver without the plugin cannot find "${PKG}"`,
          `no TS2307 for ${PKG}, so the fixture resolves it without the plugin and the ` +
            `assertions below would prove nothing. diagnostics: ${describe(diagnostics)}`
        );
      }
    } finally {
      await server.stop();
    }
  }

  {
    const server = startServer({ plugin: 'global' });
    try {
      server.open(GOOD);
      server.open(BAD);

      const good = await settle(() => server.diagnostics(GOOD), (d) => d.length === 0);
      if (good.length === 0) {
        pass(`resolved: \`import * as lib from "${PKG}"\` type-checks clean in tsserver`);
      } else {
        fail(
          `resolved: \`import * as lib from "${PKG}"\` type-checks clean in tsserver`,
          `${describe(good)}. tsserver stderr: ${server.stderr() || '(empty)'}`
        );
      }

      const bad = await settle(() => server.diagnostics(BAD), (d) => !missesPackage(d));
      const rejection = bad.find((d) => d.text.includes(BOGUS_MEMBER));
      if (!rejection) {
        fail(
          `real: lib.${BOGUS_MEMBER}() is rejected`,
          `a nonexistent member on \`lib\` produced no error, so "${PKG}" resolved to ` +
            `something untyped rather than to its declarations. diagnostics: ${describe(bad)}`
        );
      } else if (!rejection.text.includes(LIB_DECLARATIONS)) {
        fail(
          `real: lib.${BOGUS_MEMBER}() is rejected against the bazel-bin declarations`,
          `the rejection does not name ${LIB_DECLARATIONS}, so it came from somewhere ` +
            `other than the .d.ts the map names: ${rejection.text}`
        );
      } else {
        pass(`real: lib.${BOGUS_MEMBER}() is rejected against ${LIB_DECLARATIONS}`);
      }
    } finally {
      await server.stop();
    }
  }

  await disposedProjectsReleaseWorker();

  {
    const sessions = [false, 'global'].map((plugin) => ({
      server: startServer({ plugin }),
      lane: plugin ? 'plugin' : 'no-plugin',
    }));
    const consumers = new Map([
      [ordinaryConsumer, rootProject],
      [ordinarySubpathConsumer, rootProject],
      [generatedConsumer, join(workspaceRoot, generatedDirectory, 'tsconfig.json')],
    ]);
    const originalConsumers = new Map(
      [...consumers.keys()].map((file) => [file, readFileSync(file, 'utf8')])
    );
    let authoredMember = 'sourceOnly';
    let authoredType = 'number';
    try {
      for (const { server } of sessions) {
        for (const [file, project] of consumers) {
          server.open(file);
          const info = await server.request('projectInfo', { file, needFileNameList: false });
          if (realpathSync(info.configFileName) !== realpathSync(project)) {
            throw new Error(
              `entrypoint consumer bypassed its generated config: ${JSON.stringify(info)}`
            );
          }
        }
      }
      for (const phase of [
        {
          name: 'initial',
          selected: `${generatedOutput}.d.ts`,
          member: 'declarationOnly',
          type: 'number',
        },
        {
          name: 'authored edit without rebuilding',
          selected: `${generatedOutput}.d.ts`,
          member: 'declarationOnly',
          type: 'number',
          authored: true,
          writes: {
            [`${ordinaryPackage}/index.ts`]:
              'export function add(a: number, b: number): string { return String(a + b); }\n' +
              'export const addedLater = true;\n',
          },
        },
        {
          name: 'generated declaration replacement',
          selected: `${generatedOutput}.d.ts`,
          member: 'replacementOnly',
          type: 'string',
          writes: {
            [`${generatedOutput}.d.ts`]:
              'export declare function add(a: number, b: number): string;\n' +
              'export declare const replacementOnly: true;\n',
          },
        },
        {
          name: 'generated source fallback after declaration deletion',
          selected: `${generatedOutput}.ts`,
          member: 'generatedOnly',
          type: 'boolean',
          remove: `${generatedOutput}.d.ts`,
          writes: {
            [`${generatedOutput}.ts`]:
              'export function add(a: number, b: number): boolean { return true; }\n' +
              'export const generatedOnly = true;\n',
          },
        },
        { name: 'generated source deletion', remove: `${generatedOutput}.ts` },
        {
          name: 'generated source recovery',
          selected: `${generatedOutput}.ts`,
          member: 'restoredOnly',
          type: 'bigint',
          writes: {
            [`${generatedOutput}.ts`]:
              'export function add(a: number, b: number): bigint { return BigInt(a + b); }\n' +
              'export const restoredOnly = true;\n',
          },
        },
      ]) {
        for (const [file, contents] of Object.entries(phase.writes || {})) write(file, contents);
        if (phase.remove) rmSync(join(workspaceRoot, phase.remove));
        if (phase.authored) {
          authoredMember = 'addedLater';
          authoredType = 'string';
        }
        for (const { server, lane } of sessions) {
          for (const file of [ordinaryConsumer, ordinarySubpathConsumer]) {
            await checkOrdinaryState(
              server,
              file,
              `${ordinaryPackage}/index.ts`,
              authoredMember,
              authoredType,
              lane
            );
          }
          await checkOrdinaryState(
            server,
            generatedConsumer,
            phase.selected,
            phase.member,
            phase.type,
            lane
          );
          pass(`ordinary ${lane}: ${phase.name} in the same editor process ${server.pid}`);
        }
        if (
          readFileSync(join(workspaceRoot, `bazel-bin/${ordinaryPackage}/index.d.ts`), 'utf8') !==
          ordinaryDeclaration
        ) {
          throw new Error('authored editor journey replaced the stale build declaration');
        }
        for (const [file, contents] of originalConsumers) {
          if (readFileSync(file, 'utf8') !== contents)
            throw new Error(`entrypoint consumer changed: ${file}`);
        }
      }
    } finally {
      await Promise.all(sessions.map(({ server }) => server.stop()));
    }
  }

  {
    const server = startServer({ plugin: 'global' });
    try {
      for (const { file } of fragmentConsumers) server.open(file);
      server.open(authoredFragmentConsumer);
      for (const active of [
        [],
        [fragmentPackages[0].pkg],
        [fragmentPackages[1].pkg],
        fragmentPackages.map(({ pkg }) => pkg),
        [fragmentPackages[0].pkg],
      ]) {
        const phase =
          fragmentPackages
            .filter(({ pkg }) => active.includes(pkg))
            .map(({ name }) => name)
            .join('+') || 'none';
        process.stdout.write(`fragment ${phase} phase: started at ${Date.now() - startedAt}ms\n`);
        if (active.length === fragmentPackages.length) {
          const directory = dirname(join(workspaceRoot, fragmentDestination));
          const replacement = join(publicationRoot, 'fragment-directory');
          cpSync(directory, replacement, { recursive: true });
          rmSync(directory, { recursive: true });
          renameSync(replacement, directory);
        }
        publishGraph(
          [
            JSON.stringify({
              format: 'tsconfig-fragment-v1',
              label: `//${ordinaryPackage}:private`,
            }),
            ...active.map((pkg) => JSON.stringify({ package: pkg, index: true })),
          ].join('\n') + '\n',
          fragmentDestination,
          phase
        );
        await checkFragmentState(server, active, phase);
        process.stdout.write(`fragment ${phase} phase: completed at ${Date.now() - startedAt}ms\n`);
      }
    } finally {
      await server.stop();
      rmSync(join(workspaceRoot, fragmentDestination), { force: true });
    }
  }

  {
    const server = startServer({ plugin: 'tsconfig' });
    try {
      server.open(GOOD);
      const good = await settle(() => server.diagnostics(GOOD), (d) => d.length === 0);
      if (good.length === 0) {
        pass('vscode: a probe location alone loads the plugin named in tsconfig');
      } else {
        fail(
          'vscode: a probe location alone loads the plugin named in tsconfig',
          `${describe(good)} -- tsserver logs and ignores a plugin it cannot load, ` +
            'so this is what an editor that passes no --globalPlugins sees. ' +
            `tsserver stderr: ${server.stderr() || '(empty)'}`
        );
      }
      server.open(GRAPH);
      await checkGraphState(server, [], 'graph refresh: initial source packages are absent');
      const directory = join(workspaceRoot, '.bazel');
      const replacement = join(publicationRoot, 'graph-directory');
      cpSync(directory, replacement, { recursive: true });
      rmSync(directory, { recursive: true });
      renameSync(replacement, directory);
      for (const [index, { pkg }] of graphPackages.entries()) {
        publishGraph(JSON.stringify({ ...hookData, packages: [...hookData.packages, pkg] }));
        await checkGraphState(
          server,
          [pkg],
          `graph refresh: publication ${index + 1} after directory replacement updates resolution, completion and definition`
        );
      }
    } finally {
      try {
        await server.stop();
      } finally {
        try {
          publishGraph(hookContents);
        } finally {
          rmSync(publicationRoot, { recursive: true, force: true });
        }
      }
    }
  }

  if (failures > 0) {
    process.stderr.write(`\n${failures} FAILED\n`);
    process.exit(1);
  }
  process.stdout.write('\nALL PASSED\n');
}

main().catch((e) => {
  process.stderr.write(`FATAL: ${e.stack || e.message}\n`);
  process.exit(1);
});
