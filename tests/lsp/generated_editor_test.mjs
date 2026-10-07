import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { dirname, join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { startServer } from './tsserver_client.mjs';

const [, , tsserverJs, workspaceRoot, bazel, outputRoot, deadlineArg] = process.argv;
assert(
  tsserverJs && workspaceRoot && bazel && outputRoot,
  'expected tsserver, workspace, Bazel and output root'
);
const deadline = Number(deadlineArg) || 0;
const read = (file) => readFileSync(file, 'utf8');
const consumer = join(workspaceRoot, 'generated/consumer.ts');
const input = join(workspaceRoot, 'generated/names.json');
const project = join(workspaceRoot, 'generated/.bazel/tsconfig/generated_test.json');
const inheritedConsumer = join(workspaceRoot, 'native-inherited/consumer.ts');
const inheritedProject = join(workspaceRoot, 'native-inherited/tsconfig.json');
const legacyConsumer = join(workspaceRoot, 'legacy-collision/consumer.ts');
const legacyUpgradeConsumer = join(workspaceRoot, 'legacy-upgrade/consumer.ts');
const rootProject = join(workspaceRoot, 'tsconfig.json');
const originalRootConfig = read(rootProject);
const legacyEntrypoints = [
  { name: 'legacy-generated-missing' },
  { name: 'legacy-generated-stale', checkout: 'export const generatedOnly = false;\n' },
];
const legacyUpgradeImports = legacyEntrypoints.map(
  ({ name }, index) => `import * as entry${index} from "${name}";`
);
const hookDataFile = join(workspaceRoot, '.bazel/tsserver-hook-data.json');
const originalHookData = read(hookDataFile);
const collisionNames = ['zod', 'unresolved-package'];
const legacyImports = collisionNames.map(
  (name, index) => `import * as package${index} from "${name}";`
);
const originalConsumer = read(consumer);
const originalInput = read(input);
const members = ['removed', 'nested'];
const specifiers = { removed: 'authored', nested: 'authored/nested' };
const probes = [
  'import { generatedMembers } from "#generated/value";',
  'import { generatedTreeMembers } from "#types/first";',
  'import * as nativeNpm from "zod";',
  'export const nativeSchema = nativeNpm.string();',
  'import * as unavailable from "unresolved-package";',
  'export const missingValue = unavailable.firstPartyOnly;',
  ...members.map((member) =>
    [
      `import { ${member} as ${member}Child, generatedTreeMembers as ${member}Members } from "${specifiers[member]}";`,
      `export type ${member}Type = typeof ${member}Child;`,
      `export const scalar_${member} = generatedMembers.${member};`,
      `export const tree_${member} = generatedTreeMembers.${member};`,
      `export const alias_${member} = ${member}Members.${member};`,
    ].join('\n')
  ),
].join('\n');
const source = originalConsumer + '\n' + probes + '\n';
const preserved = new Map(
  [
    '.bazel/tsserver-hook-data.json',
    'generated/tsconfig.json',
    'generated/tsconfig.build.json',
    'generated/package.json',
    'generated/BUILD.bazel',
    'generated/generate.mjs',
    'generated/override.ts',
    'generated/authored/value.ts',
    'generated/generated/value.ts',
    'generated/generated/types/removed.d.ts',
    'generated/generated/schema.json',
    'authored/value.ts',
    'authored/index.ts',
    'authored/nested/index.ts',
    'authored/settings.json',
    'authored/tsconfig.json',
    'authored/package.json',
  ].map((rel) => {
    const file = join(workspaceRoot, rel);
    return [file, read(file)];
  })
);

function refresh(phase) {
  process.stdout.write(`editor ${phase}: refresh started\n`);
  if (deadline) assert(Date.now() < deadline, 'enclosing test deadline exhausted before refresh');
  // tsserver stops watching a file it finds missing mid-replacement and recreated before its
  // fallback poller's first stat; a cache hit replaces outputs that fast, so hold it until done.
  const paused = sessions.filter(({ server }) => server?.alive());
  for (const { server } of paused) process.kill(server.pid, 'SIGSTOP');
  let result;
  try {
    result = spawnSync(
      bazel,
      [
        'run',
        '--symlink_prefix=bazel-editor-refresh-',
        '--run_validations=false',
        '--output_groups=-_validation',
        '//generated:refresh_generated',
      ],
      {
        cwd: workspaceRoot,
        stdio: 'inherit',
        ...(deadline ? { timeout: Math.max(1, deadline - Date.now()) } : {}),
      }
    );
  } finally {
    for (const { server } of paused) process.kill(server.pid, 'SIGCONT');
  }
  assert.ifError(result.error);
  assert.equal(result.status, 0, 'actual generated_sources refresh failed');
  process.stdout.write(`editor ${phase}: refresh completed\n`);
}

function probeSpan(text, advance = 0) {
  const index = probes.indexOf(text);
  assert(index >= 0, `probe is missing: ${text}`);
  assert.equal(probes.indexOf(text, index + 1), -1, `probe is ambiguous: ${text}`);
  return { start: originalConsumer.length + 1 + index + advance, length: text.length - advance };
}

function position(span, advance = 0, file = consumer) {
  const before = source.slice(0, span.start + advance).split('\n');
  return { file, line: before.length, offset: before.at(-1).length + 1 };
}

function assertSources(expectedConsumer) {
  assert.equal(read(consumer), expectedConsumer, 'refresh changed the open consumer');
  for (const [file, contents] of preserved) {
    assert.equal(read(file), contents, `refresh changed authored or stale checkout bytes: ${file}`);
  }
}

function propertyNamePattern(member) {
  return `(?:^|[\\s{;.])(?:${member}|${JSON.stringify(member)})`;
}

function assertGeneratedInputs(phase, present) {
  const directory = join(outputRoot, 'generated/generated');
  function identity(file) {
    const canonical = realpathSync(file);
    const { dev, ino, size, mtimeMs, ctimeMs } = statSync(canonical);
    return { path: file, canonical, dev, ino, size, mtimeMs, ctimeMs };
  }
  const outputs = [
    ['value.d.ts', 'generatedMembers', true],
    ...['first', ...members].map((member) => [
      `types/${member}.d.ts`,
      'generatedTreeMembers',
      present.includes(member),
    ]),
  ].map(([output, object, expected]) => {
    const file = join(directory, output);
    if (!existsSync(file)) return { output, expected, exists: false, path: file };
    const observed = identity(file);
    const declaration = read(observed.canonical).match(
      new RegExp(`\\b${object}\\s*:\\s*\\{([\\s\\S]*?)\\}`)
    );
    const actual = declaration
      ? [...declaration[1].matchAll(/\breadonly\s+(?:"([^"]+)"|([\w$]+))\s*:/g)]
          .map((match) => match[1] || match[2])
          .sort()
      : undefined;
    return { output, expected, exists: true, ...observed, members: actual };
  });
  process.stdout.write(
    `editor ${phase}: generated inputs ${JSON.stringify({
      directories: [identity(directory), identity(join(directory, 'types'))],
      outputs,
    })}\n`
  );
  for (const output of outputs) {
    assert.equal(output.exists, output.expected, `${phase}: generated ${output.output} presence`);
    if (output.expected) {
      assert.deepEqual(
        output.members,
        [...present].sort(),
        `${phase}: materialized ${output.canonical} has stale generated members`
      );
    }
  }
}

const stalledAssertionMs = 30_000;
const nativeResults = new Map();

async function checkNativeNamespace(request, diagnostics, file, plugin) {
  const npmSpan = probeSpan('from "zod"', 'from '.length);
  const definitions = (await request('definition', position(npmSpan, 2, file))) || [];
  const npmRoot = join(outputRoot, 'node_modules/zod');
  const expected = realpathSync(
    join(npmRoot, JSON.parse(read(join(npmRoot, 'package.json'))).types)
  );
  const npmDefinitions = definitions.map((entry) => realpathSync(entry.file)).sort();
  assert(
    npmDefinitions.length > 0 && npmDefinitions.every((path) => path === expected),
    `npm definition escaped the declared package: ${JSON.stringify(definitions)}`
  );
  const completion = await request(
    'completionInfo',
    position(probeSpan('nativeNpm.string', 'nativeNpm.'.length), 0, file)
  );
  const npmMembers = (completion?.entries || []).map((entry) => entry.name).sort();
  assert(
    npmMembers.includes('string') && !npmMembers.includes('firstPartyOnly'),
    'npm completions were replaced by an unrelated workspace package'
  );
  assert(
    !diagnostics.some(
      (diagnostic) => diagnostic.code === 2307 && diagnostic.start === npmSpan.start
    ),
    'declared npm dependency stopped resolving'
  );
  const missingSpan = probeSpan('from "unresolved-package"', 'from '.length);
  const unresolved = diagnostics.filter(
    (diagnostic) =>
      diagnostic.code === 2307 &&
      diagnostic.start === missingSpan.start &&
      diagnostic.length === missingSpan.length
  );
  assert.equal(unresolved.length, 1, 'worker made an unresolved non-alias import resolve');
  assert.equal(
    ((await request('definition', position(missingSpan, 2, file))) || []).length,
    0,
    'unresolved import navigated to an unrelated workspace package'
  );
  const result = {
    npmDefinitions,
    npmMembers,
    unresolved: unresolved.map(({ code, start, length }) => ({ code, start, length })),
  };
  if (plugin)
    assert.deepEqual(
      result,
      nativeResults.get(file),
      'plugin changed the native resolution namespace'
    );
  else nativeResults.set(file, result);
}

async function checkEditor({ server, name, plugin }, present, trace) {
  async function request(command, args) {
    const at = args.line ? ` at ${args.line}:${args.offset}` : '';
    if (trace) process.stdout.write(`editor ${name} request ${command}${at}: started\n`);
    const result = await server.request(command, args);
    if (trace) process.stdout.write(`editor ${name} request ${command}${at}: completed\n`);
    return result;
  }
  const info = await request('projectInfo', { file: consumer, needFileNameList: true });
  assert.equal(
    info.configFileName,
    project,
    'authored solution did not select its generated editor project'
  );
  if (plugin) {
    assert(
      server
        .stderr()
        .split('\n')
        .some((line) => line.startsWith('[tsserver-plugin]') && line.includes(project)),
      'installed plugin did not load on the generated editor project'
    );
    for (const specifier of [...Object.values(specifiers), ...collisionNames]) {
      assert(
        server
          .stderr()
          .includes(
            `[tsserver-hook-worker] internal (src): ${specifier} → ${join(workspaceRoot, specifier, 'index.ts')}`
          ),
        `worker did not discover the competing checkout entry for ${specifier}`
      );
    }
    assert(
      server.stderr().includes('[tsserver-hook] resolution map ready:'),
      'plugin did not receive the competing worker map'
    );
    for (const [index, specifier] of collisionNames.entries()) {
      const definitions =
        (await request('definition', {
          file: legacyConsumer,
          line: index + 1,
          offset: legacyImports[index].indexOf('"') + 2,
        })) || [];
      const expected = realpathSync(join(workspaceRoot, specifier, 'index.ts'));
      assert(
        definitions.length > 0 &&
          definitions.every((entry) => realpathSync(entry.file) === expected),
        `legacy project did not use the competing worker entry for ${specifier}`
      );
    }
    assert.deepEqual(
      await request('semanticDiagnosticsSync', { file: legacyUpgradeConsumer }),
      [],
      'native refresh broke an N-1 generated exact import'
    );
    for (const [index, { name }] of legacyEntrypoints.entries()) {
      const definitions =
        (await request('definition', {
          file: legacyUpgradeConsumer,
          line: index + 1,
          offset: legacyUpgradeImports[index].indexOf('"') + 2,
        })) || [];
      const expected = realpathSync(join(outputRoot, name, 'index.d.ts'));
      assert(
        definitions.length > 0 &&
          definitions.every((entry) => realpathSync(entry.file) === expected),
        `native refresh lost the N-1 generated definition for ${name}`
      );
    }
  }
  const diagnostics = await request('semanticDiagnosticsSync', {
    file: consumer,
    includeLinePosition: true,
  });
  assert(
    Array.isArray(diagnostics) &&
      diagnostics.every((d) => Number.isInteger(d.start) && Number.isInteger(d.length)),
    'malformed synchronous diagnostics'
  );
  await checkNativeNamespace(request, diagnostics, consumer, plugin);
  const inheritedInfo = await request('projectInfo', {
    file: inheritedConsumer,
    needFileNameList: false,
  });
  assert.equal(
    inheritedInfo.configFileName,
    inheritedProject,
    'inherited compiler project was not selected'
  );
  const inheritedDiagnostics = await request('semanticDiagnosticsSync', {
    file: inheritedConsumer,
    includeLinePosition: true,
  });
  await checkNativeNamespace(request, inheritedDiagnostics, inheritedConsumer, plugin);
  for (const object of ['generatedMembers', 'generatedTreeMembers']) {
    const hover = await request('quickinfo', position(probeSpan(`${object}.${members[0]}`)));
    for (const member of ['first', ...members]) {
      assert.equal(
        new RegExp(`${propertyNamePattern(member)}\\??:`).test(hover?.displayString || ''),
        present.includes(member),
        `${object}: hover retained stale membership: ${JSON.stringify(hover)}`
      );
    }
  }
  for (const member of members) {
    const active = present.includes(member);
    for (const [object, output] of [
      ['generatedMembers', 'value.d.ts'],
      ['generatedTreeMembers', 'types/first.d.ts'],
      [`${member}Members`, `types/${member}.d.ts`],
    ]) {
      const span = probeSpan(`${object}.${member}`, object.length + 1);
      const at = position(span);
      const completion = await request('completionInfo', at);
      assert.equal(
        completion?.entries.some((entry) => entry.name === member) || false,
        active,
        `${object}.${member}: completion membership`
      );
      const inside = { ...at, offset: at.offset + 1 };
      const definitions = (await request('definition', inside)) || [];
      if (active) {
        const expected = realpathSync(join(outputRoot, 'generated/generated', output));
        assert(
          definitions.length > 0 &&
            definitions.every((entry) => realpathSync(entry.file) === expected),
          `${object}.${member}: definition escaped canonical generated output: ${JSON.stringify(definitions)}`
        );
        const hover = await request('quickinfo', inside);
        const type = object === 'generatedMembers' ? JSON.stringify(member) : 'string';
        assert(
          hover?.kind === 'property' &&
            new RegExp(`${propertyNamePattern(member)}: ${type}$`).test(hover.displayString),
          `${object}.${member}: hover lost the generated type: ${JSON.stringify(hover)}`
        );
      } else {
        assert.equal(definitions.length, 0, `${object}.${member}: deleted definition remains`);
      }
      const rejected = diagnostics.some(
        (d) => d.code === 2339 && d.start === span.start && d.length === span.length
      );
      if (object !== `${member}Members`) {
        assert.equal(
          rejected,
          !active,
          `${object}.${member}: deletion diagnostic: ${JSON.stringify(diagnostics)}`
        );
      }
    }
    const specifier = specifiers[member];
    const quotedSpecifier = JSON.stringify(specifier);
    const span = probeSpan(`from ${quotedSpecifier}`, 'from '.length);
    const at = position(span, 3);
    const definitions = (await request('definition', at)) || [];
    if (active) {
      const expected = realpathSync(
        join(outputRoot, 'generated/generated/types', `${member}.d.ts`)
      );
      assert(
        definitions.length > 0 &&
          definitions.every((entry) => realpathSync(entry.file) === expected),
        `${specifier}: tree-child navigation did not reach the generated declaration`
      );
    } else {
      assert.equal(definitions.length, 0, `${specifier}: deleted tree child still resolves`);
    }
    assert.equal(
      diagnostics.some(
        (d) => d.code === 2307 && d.start === span.start && d.length === span.length
      ),
      !active,
      `${specifier}: deletion diagnostic: ${JSON.stringify(diagnostics)}`
    );
  }
  if (!plugin) {
    assert(
      !server.stderr().includes('[tsserver-plugin]') && !server.stderr().includes('[tsserver-hook'),
      'no-plugin session loaded the plugin or its watcher'
    );
  }
}

const sessions = [
  { name: 'no-plugin', plugin: false },
  { name: 'plugin', plugin: 'global' },
];
let failure;
const createdDirectories = [];
const fixtureFiles = new Map([
  ...collisionNames.flatMap((name) => [
    [join(workspaceRoot, name, 'BUILD.bazel'), 'exports_files(["index.ts"])\n'],
    [
      join(workspaceRoot, name, 'index.ts'),
      'export function string() { return "workspace"; }\nexport const firstPartyOnly = true;\n',
    ],
  ]),
  ...legacyEntrypoints.flatMap(({ name, checkout }) => [
    [join(workspaceRoot, name, 'BUILD.bazel'), ''],
    [join(outputRoot, name, 'index.d.ts'), 'export declare const generatedOnly: true;\n'],
    ...(checkout ? [[join(workspaceRoot, name, 'index.ts'), checkout]] : []),
  ]),
  [
    legacyUpgradeConsumer,
    legacyUpgradeImports.join('\n') +
      '\n' +
      legacyEntrypoints
        .map((_, index) => `export const expected${index}: true = entry${index}.generatedOnly;\n`)
        .join(''),
  ],
  [
    legacyConsumer,
    legacyImports.join('\n') +
      '\nexport const values = [package0.firstPartyOnly, package1.firstPartyOnly];\n',
  ],
  [
    join(workspaceRoot, 'legacy-collision/tsconfig.json'),
    JSON.stringify({
      compilerOptions: { module: 'Preserve', moduleResolution: 'Bundler', noEmit: true },
      files: ['consumer.ts'],
    }) + '\n',
  ],
  [inheritedConsumer, source],
  [
    inheritedProject,
    JSON.stringify({
      extends: '../generated/.bazel/tsconfig/generated_test.json',
      files: ['consumer.ts'],
    }) + '\n',
  ],
]);
try {
  process.stdout.write('editor session: checking runtime and installed plugin\n');
  assert.equal(JSON.parse(read(join(dirname(tsserverJs), '../package.json'))).version, '5.9.2');
  for (const directory of [
    ...[...collisionNames, 'legacy-collision', 'legacy-upgrade', 'native-inherited'].map((name) =>
      join(workspaceRoot, name)
    ),
    ...legacyEntrypoints.flatMap(({ name }) => [join(workspaceRoot, name), join(outputRoot, name)]),
  ]) {
    assert(!existsSync(directory), `collision fixture already exists: ${directory}`);
    mkdirSync(directory);
    createdDirectories.push(directory);
  }
  for (const [file, contents] of fixtureFiles) {
    writeFileSync(file, contents);
    preserved.set(file, contents);
  }
  const legacyConfig = JSON.parse(originalRootConfig);
  delete legacyConfig._rules_typescript;
  for (const { name } of legacyEntrypoints) {
    legacyConfig.compilerOptions.paths[name] = [`./${name}/index`];
    legacyConfig.compilerOptions.paths[`${name}/*`] = [`./${name}/*`, `./bazel-bin/${name}/*`];
  }
  const legacyConfigBytes = JSON.stringify(legacyConfig, null, 2) + '\n';
  writeFileSync(rootProject, legacyConfigBytes);
  preserved.set(rootProject, legacyConfigBytes);
  const hookData = JSON.parse(originalHookData);
  const collidingHookData =
    JSON.stringify({
      ...hookData,
      packages: [
        ...new Set([
          ...hookData.packages,
          ...collisionNames,
          ...legacyEntrypoints.map(({ name }) => name),
        ]),
      ],
    }) + '\n';
  writeFileSync(hookDataFile, collidingHookData);
  preserved.set(hookDataFile, collidingHookData);
  const installedPlugin = new Map(
    ['index.js', 'package.json', 'tsserver-hook-resolver.js', 'tsserver-hook-worker.js'].map(
      (name) => {
        const file = join(
          workspaceRoot,
          '.bazel/node_modules/@rules_typescript/tsserver-plugin',
          name
        );
        assert(lstatSync(file).isFile(), `ordinary refresh did not copy plugin file ${file}`);
        return [file, read(file)];
      }
    )
  );
  for (const file of installedPlugin.keys()) {
    writeFileSync(file, 'stale plugin installation\n');
  }
  writeFileSync(consumer, source);
  const phases = [
    ['initial', ['first', 'removed']],
    ['add', ['first', 'removed', 'nested']],
    ['delete', ['first']],
    ['recreate', ['first', 'removed', 'nested']],
  ];
  for (const [phase, names] of phases) {
    writeFileSync(input, JSON.stringify(names) + '\n');
    process.stdout.write(`editor phase ${phase}: generator input ${names.join(',')}\n`);
    refresh(phase);
    assertSources(source);
    assertGeneratedInputs(phase, names);
    for (const [file, contents] of installedPlugin) {
      assert.equal(read(file), contents, `native refresh retained stale plugin bytes: ${file}`);
    }
    assert.equal(
      JSON.parse(read(project)).compilerOptions.moduleResolution?.toLowerCase(),
      'bundler',
      'generated editor project must explicitly preserve native Bundler resolution'
    );
    const installedProject = JSON.parse(read(project));
    assert.equal(
      installedProject._rules_typescript,
      'compiler-project',
      'compiler project omitted its ownership identity'
    );
    for (const specifier of collisionNames) {
      assert(
        !Object.keys(installedProject.compilerOptions.paths).some((key) => {
          if (!key.includes('*')) return key === specifier;
          const [prefix, suffix] = key.split('*');
          return specifier.startsWith(prefix) && specifier.endsWith(suffix);
        }),
        `${specifier} must exercise native resolution outside projected aliases`
      );
    }
    for (const session of sessions) {
      if (!session.server) {
        session.server = startServer({
          tsserverJs,
          workspaceRoot,
          plugin: session.plugin,
          deadline,
          env: { TSSERVER_HOOK_DEBUG: '1', BAZEL: bazel },
        });
        session.pid = session.server.pid;
        if (session.plugin) {
          session.server.open(legacyConsumer);
          session.server.open(legacyUpgradeConsumer);
        }
        session.server.open(consumer);
        session.server.open(inheritedConsumer);
        process.stdout.write(
          `editor ${session.name} session: opened consumer in process ${session.pid}\n`
        );
      }
    }
    for (const session of sessions) {
      const { server, name, pid } = session;
      if (session.plugin) {
        const legacyInfo = await server.request('projectInfo', {
          file: legacyUpgradeConsumer,
          needFileNameList: false,
          needDefaultConfiguredProjectInfo: true,
        });
        if (legacyInfo.configFileName !== rootProject) {
          process.stderr.write(
            `editor ${name} ${phase}: legacy project ${JSON.stringify(legacyInfo)}\n`
          );
          try {
            process.stderr.write(`editor installed ${rootProject}:\n${read(rootProject)}\n`);
            const rootInfo = await server.request('projectInfo', {
              file: legacyUpgradeConsumer,
              projectFileName: rootProject,
              needFileNameList: true,
            });
            process.stderr.write(`editor root project: ${JSON.stringify(rootInfo)}\n`);
          } catch (error) {
            process.stderr.write(
              `editor root project diagnostic failed: ${error.stack || error}\n`
            );
          }
        }
        assert.equal(
          legacyInfo.configFileName,
          rootProject,
          'legacy consumer bypassed its N-1 config'
        );
      }
      let last;
      let since;
      let trace = true;
      do {
        assert(
          server.alive() && server.pid === pid,
          `${name} ${phase}: tsserver process changed or exited`
        );
        try {
          await checkEditor(session, names, trace);
          last = undefined;
          break;
        } catch (error) {
          if (!(error instanceof assert.AssertionError)) throw error;
          trace = error.message !== last?.message;
          if (trace) {
            since = Date.now();
            process.stdout.write(
              `editor ${name} phase ${phase}: awaiting assertion: ${error.message}\n`
            );
          }
          last = error;
          // Settling takes a few watcher polls; an unchanged failure past this is a missed change.
          if (Date.now() - since > stalledAssertionMs) break;
        }
        await delay(250);
      } while (!deadline || Date.now() < deadline);
      if (last) throw new Error(`${name} ${phase}: ${last.message}`, { cause: last });
      assertSources(source);
      process.stdout.write(
        `editor ${name} phase ${phase}: same process ${pid}, unchanged consumer, completion/navigation/hover/deletion checks passed\n`
      );
    }
    process.stdout.write(
      `editor phase ${phase}: both TypeScript 5.9 lanes passed with unchanged consumer\n`
    );
  }
} catch (error) {
  failure = error;
  process.stderr.write(`editor primary failure: ${error.stack || error}\n`);
} finally {
  for (const { server, name } of sessions) {
    if (!server) continue;
    try {
      process.stdout.write(`editor ${name} cleanup: tsserver exit started\n`);
      await server.stop();
      process.stdout.write(`editor ${name} cleanup: tsserver exit completed\n`);
    } catch (error) {
      process.stderr.write(`editor ${name} cleanup failure: ${error.stack || error}\n`);
      failure ??= error;
    }
  }
  try {
    writeFileSync(consumer, originalConsumer);
    writeFileSync(input, originalInput);
    writeFileSync(rootProject, originalRootConfig);
    preserved.set(rootProject, originalRootConfig);
    writeFileSync(hookDataFile, originalHookData);
    preserved.set(hookDataFile, originalHookData);
    for (const directory of createdDirectories) rmSync(directory, { recursive: true, force: true });
    for (const file of fixtureFiles.keys()) preserved.delete(file);
    process.stdout.write('editor cleanup: original consumer and generator input restored\n');
    refresh('cleanup');
    assert.equal(read(input), originalInput, 'generator input restoration failed');
    assertSources(originalConsumer);
    process.stdout.write('editor fixture restored and generated_sources refreshed\n');
  } catch (error) {
    process.stderr.write(`editor restoration failure: ${error.stack || error}\n`);
    failure ??= error;
  }
}
if (failure) throw failure;
