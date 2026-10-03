// One fixture graph, two resolution modes: each specifier must land on the same
// module in dev and build. A RegExp alias: a string `find` matches by prefix.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const bundlePath = process.argv[2];
if (!bundlePath || !fs.existsSync(bundlePath)) {
  process.stderr.write(`FATAL: bundle not found: ${bundlePath}\n`);
  process.exit(1);
}

const { BazelResolver, bazelPlugin } = await import(pathToFileURL(bundlePath).href);
const { createServer } = await import(pathToFileURL(createRequire(process.argv[3]).resolve('vite')).href);

// ── The fixture graph ───────────────────────────────────────────────────────

const root = fs.mkdtempSync(path.join(process.env.TEST_TMPDIR || os.tmpdir(), 'parity-'));
const workspaceRoot = path.join(root, 'ws');
const bazelBin = path.join(workspaceRoot, 'bazel-bin');

const write = (file, content) => {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content);
};

// Checked-in source.
write(
  path.join(workspaceRoot, 'app/main.ts'),
  'import { help } from "./helper.js";\n' +
    'import { name } from "@fixture/lib";\n' +
    'import leftpad from "leftpad";\n' +
    'export const app = [help, name, leftpad];\n',
);
write(path.join(workspaceRoot, 'app/helper.ts'), 'export const help = "help";\n');
write(path.join(workspaceRoot, 'packages/lib/index.ts'), 'export const name = "@fixture/lib";\n');

// What Bazel compiled from it.
write(path.join(bazelBin, 'app/main.js'), 'export const app = [];\n');
write(path.join(bazelBin, 'app/helper.js'), 'export const help = "help";\n');
write(path.join(bazelBin, 'packages/lib/index.js'), 'export const name = "@fixture/lib";\n');

// The npm tree Bazel built.
write(path.join(workspaceRoot, 'node_modules/leftpad/package.json'), '{"name":"leftpad"}\n');
write(path.join(workspaceRoot, 'node_modules/leftpad/index.js'), 'export default () => "";\n');

// ── The alias table ts_dev_server generates ─────────────────────────────────

const escape = (name) => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const aliases = [
  {
    find: new RegExp('^' + escape('@fixture/lib') + '$'),
    replacement: path.join(workspaceRoot, 'packages/lib/index.ts'),
  },
  { find: '@fixture/lib', replacement: path.join(workspaceRoot, 'packages/lib') },
];

/** Vite's own alias matching: exact, or the prefix followed by a slash. */
function applyAlias(specifier) {
  for (const { find, replacement } of aliases) {
    if (find instanceof RegExp) {
      if (find.test(specifier)) return specifier.replace(find, replacement);
      continue;
    }
    if (specifier === find) return replacement;
    if (specifier.startsWith(find + '/')) return replacement + specifier.slice(find.length);
  }
  return specifier;
}

// ── Resolution, in both modes ───────────────────────────────────────────────

// Each mode's real importer: dev serves the .ts entry, prod links the .js Bazel
// compiled from it.
const importers = {
  serve: path.join(workspaceRoot, 'app/main.ts'),
  build: path.join(bazelBin, 'app/main.js'),
};

function resolve(mode, specifier) {
  const resolver = new BazelResolver({ workspaceRoot, bazelBin, mode });
  return resolver.resolveId(applyAlias(specifier), importers[mode]);
}

/**
 * The module a resolution names, independent of which root it came out of and
 * which extension that root spells it with. Two resolutions with the same
 * identity are the same module of the graph.
 */
function identity(resolution) {
  if (resolution === null) return null;
  const file = resolution.filePath;
  const rel = file.startsWith(bazelBin + path.sep)
    ? path.relative(bazelBin, file)
    : path.relative(workspaceRoot, file);
  return rel.replace(/\.(tsx?|jsx?)$/, '');
}

const tests = [];
const test = (name, fn) => tests.push([name, fn]);

test('a first-party bare specifier is one module in both modes', () => {
  const serve = resolve('serve', '@fixture/lib');
  const build = resolve('build', '@fixture/lib');

  assert.equal(identity(serve), 'packages/lib/index');
  assert.equal(
    identity(build),
    identity(serve),
    `@fixture/lib is ${identity(build)} in prod and ${identity(serve)} in dev`,
  );

  // Same module, different owner: Vite transforms the source, Bazel already
  // transformed the output.
  assert.equal(serve.precompiled, false);
  assert.equal(build.precompiled, true);
  assert.ok(serve.filePath.endsWith('.ts'));
  assert.ok(build.filePath.endsWith('.js'));
});

test('a relative ./foo.js import is one module in both modes', () => {
  const serve = resolve('serve', './helper.js');
  const build = resolve('build', './helper.js');

  // TypeScript's node16 ESM output spells `./helper.ts` as `./helper.js` in the
  // source text, so dev has to invert it and prod does not.
  assert.equal(identity(serve), 'app/helper');
  assert.equal(
    identity(build),
    identity(serve),
    `./helper.js is ${identity(build)} in prod and ${identity(serve)} in dev`,
  );
  assert.equal(serve.precompiled, false);
  assert.equal(build.precompiled, true);

  for (const extension of ['ts', 'tsx']) {
    const logical = `app/overlap-${extension}`;
    const source = path.join(workspaceRoot, `${logical}.${extension}`);
    const compiled = path.join(bazelBin, `${logical}.js`);
    const asset = path.join(bazelBin, `published-assets/overlap-${extension}.js`);
    const sentinel = `export const value = "checked ${extension} source";\n`;
    write(source, sentinel);
    write(compiled, sentinel);
    write(asset, 'export const value = "standalone JavaScript asset";\n');
    const declaredFiles = {
      'app/main.ts': { path: importers.serve, context: 'source' },
      [`${logical}.${extension}`]: { path: source, context: 'source' },
      [`${logical}.js`]: { path: asset, context: 'asset' },
    };
    const sourceResolver = new BazelResolver({ workspaceRoot, bazelBin, mode: 'serve', declaredFiles });
    const buildResolver = new BazelResolver({ workspaceRoot, bazelBin, mode: 'build', declaredFiles });
    const specifier = `./overlap-${extension}.js`;
    const selectedSource = sourceResolver.resolveId(specifier, importers.serve);
    const selectedBuild = buildResolver.resolveId(specifier, importers.build);
    assert.deepEqual(selectedSource, { filePath: source, precompiled: false, mapPath: null },
      `a declared JavaScript asset replaced the ${extension} source`);
    assert.deepEqual(selectedBuild, { filePath: compiled, precompiled: true, mapPath: null });
    assert.equal(identity(selectedSource), logical);
    assert.equal(identity(selectedBuild), logical);
    assert.equal(fs.readFileSync(selectedSource.filePath, 'utf8'), sentinel);
    assert.equal(fs.readFileSync(selectedBuild.filePath, 'utf8'), sentinel);
    assert.equal(sourceResolver.resolveId(asset)?.filePath, asset);
    assert.equal(sourceResolver.resolveId('./overlap-ts.js', asset), null,
      'JavaScript assets retain their physical relative import context');
  }
});

test('an npm bare specifier is left to Vite in both modes', () => {
  // Neither mode may capture it, and the alias table must not swallow it: npm
  // resolution is Vite's, out of the node_modules tree Bazel built.
  assert.equal(applyAlias('leftpad'), 'leftpad');
  assert.equal(identity(resolve('serve', 'leftpad')), null);
  assert.equal(identity(resolve('build', 'leftpad')), null);
});

test('a bare specifier that only shares a prefix is not swallowed', () => {
  // A string `find` in Vite's alias plugin matches `find` and `find/...` only,
  // which is why the exact entry is a RegExp: `@fixture/libish` must not become
  // `<pkg>/index.ts` + "ish".
  assert.equal(applyAlias('@fixture/libish'), '@fixture/libish');
  assert.equal(identity(resolve('serve', '@fixture/libish')), null);
  assert.equal(identity(resolve('build', '@fixture/libish')), null);
});

test('a first-party subpath is delegated in both modes', () => {
  // The alias rewrites `@fixture/lib/button` to an extensionless path under the
  // package. Neither mode claims it, so Vite's own extension probing decides --
  // the same probing in dev and in prod.
  assert.equal(applyAlias('@fixture/lib/button'), path.join(workspaceRoot, 'packages/lib/button'));
  assert.equal(identity(resolve('serve', '@fixture/lib/button')), null);
  assert.equal(identity(resolve('build', '@fixture/lib/button')), null);
});

test('a ts_codegen output resolves to bazel-bin in dev, and nowhere else', () => {
  // Generated source has no checked-in counterpart, so it is the one case where
  // dev must still read bazel-bin. Prod links the compiled .js of the same file.
  write(path.join(bazelBin, 'app/routes.gen.ts'), 'export const routes = [];\n');
  write(path.join(bazelBin, 'app/routes.gen.js'), 'export const routes = [];\n');

  const serve = resolve('serve', './routes.gen.js');
  const build = resolve('build', './routes.gen.js');
  assert.equal(identity(serve), 'app/routes.gen');
  assert.equal(identity(build), identity(serve));
  assert.ok(serve.filePath.startsWith(bazelBin + path.sep), 'dev must read generated code from bazel-bin');
});

test('a canonical importer in a symlinked workspace still resolves generated output', () => {
  write(path.join(bazelBin, 'app/routes.gen.ts'), 'export const routes = [];\n');
  write(path.join(bazelBin, 'app/routes.gen.js'), 'export const routes = [];\n');
  const linkedWorkspace = path.join(root, 'linked-ws');
  fs.symlinkSync(workspaceRoot, linkedWorkspace, 'dir');
  const importer = fs.realpathSync(path.join(workspaceRoot, 'app/main.ts'));
  for (const mode of ['serve', 'build']) {
    const resolver = new BazelResolver({ workspaceRoot: linkedWorkspace, bazelBin, mode });
    const result = resolver.resolveId('./routes.gen.ts', importer);
    assert.equal(result?.filePath, path.join(bazelBin, `app/routes.gen.${mode === 'serve' ? 'ts' : 'js'}`));
    assert.deepEqual(resolver.resolveId('./routes.gen.ts', path.join(linkedWorkspace, 'app/main.ts')), result);
    assert.equal(resolver.resolveId('./routes.gen.ts', path.join(root, 'outside/main.ts')), null);
  }
});

test('a moved package asset keeps its exact source authority without a source alias', () => {
  for (const [name, original] of [
    ['generated', path.join(bazelBin, 'assets/generated.css')],
    ['authored', path.join(workspaceRoot, 'assets/authored.css')],
    ['external', path.join(root, 'external-source/theme.css')],
  ]) {
    const logical = `app/assets/${name}.css`;
    write(original, 'body { color: red; }\n');
    write(path.join(bazelBin, `app/${logical}`), 'body { color: red; }\n');
    const resolver = new BazelResolver({
      workspaceRoot, bazelBin, mode: 'serve',
      declaredFiles: { [logical]: { path: original, context: 'asset' } },
    });
    assert.equal(fs.existsSync(path.join(workspaceRoot, logical)), false);
    assert.equal(fs.existsSync(path.join(bazelBin, logical)), false);
    assert.deepEqual(resolver.resolveId(`./assets/${name}.css`, importers.serve), {
      filePath: original, precompiled: false, mapPath: null,
    });
    assert.equal(resolver.resolveId(`./assets/${name}.css?raw`, importers.serve)?.filePath, `${original}?raw`);
    assert.equal(resolver.resolveId('./assets/absent.css', importers.serve), null);
    assert.equal(resolver.resolveId('../helper.js', original), null,
      'published assets retain their physical relative import context');
    const rootAsset = new BazelResolver({
      workspaceRoot, bazelBin, mode: 'serve',
      declaredFiles: { ['__proto__']: { path: original, context: 'asset' } },
    });
    assert.equal(rootAsset.resolveId(path.join(workspaceRoot, '__proto__'))?.filePath, original);
    const plugin = bazelPlugin({
      workspaceRoot, bazelBin, mode: 'serve',
      declaredFiles: { [logical]: { path: original, context: 'asset' } },
    });
    const context = { environment: { config: {
      root: path.join(workspaceRoot, 'app'), resolve: { extensions: [] },
    } } };
    plugin.configResolved({ root: context.environment.config.root, logger: { info() {} } });
    write(path.join(workspaceRoot, logical), 'body { color: green; }\n');
    assert.equal(plugin.resolveId.handler.call(context, `/assets/${name}.css?raw`, undefined),
      original + '?raw', 'a browser-root asset selected checkout bytes');
    fs.unlinkSync(original);
    assert.equal(plugin.resolveId.handler.call(context, `/assets/${name}.css?raw`, undefined),
      original + '?raw', 'a missing browser-root asset fell back to checkout');
  }
});

test('a declared asset cannot be replaced by an unrelated checkout file', () => {
  const logical = 'app/assets/unchanged.css';
  const canonical = path.join(bazelBin, logical);
  write(canonical, 'body { color: blue; }\n');
  const resolver = new BazelResolver({
    workspaceRoot, bazelBin, mode: 'serve',
    declaredFiles: { [logical]: { path: canonical, context: 'asset' } },
  });
  assert.equal(resolver.resolveId('./assets/unchanged.css', importers.serve)?.filePath, canonical);
  const previous = new BazelResolver({ workspaceRoot, bazelBin, mode: 'serve' });
  assert.deepEqual(previous.resolveId('./assets/unchanged.css', importers.serve),
    resolver.resolveId('./assets/unchanged.css', importers.serve));
  write(path.join(workspaceRoot, logical), 'body { color: green; }\n');
  const selected = resolver.resolveId('./assets/unchanged.css', importers.serve);
  assert.equal(selected?.filePath, canonical);
  assert.equal(fs.readFileSync(selected.filePath, 'utf8'), 'body { color: blue; }\n');
  assert.equal(previous.resolveId('./assets/unchanged.css', importers.serve), null,
    'without a declared asset mapping, the checkout file remains Vite-owned');
});

test('package imports cannot replace declared scope, target, conditions, query or module metadata', async () => {
  const logical = 'package-scope/value.ts';
  const original = path.join(bazelBin, logical);
  const checkout = path.join(workspaceRoot, logical);
  const target = path.join(root, 'package-scope-generated.ts');
  const authored = path.join(workspaceRoot, 'package-scope/nested/main.ts');
  const manifest = path.join(workspaceRoot, 'package-scope/package.json');
  const mainScope = path.join(workspaceRoot, 'package.json');
  const externalScope = path.join(root, 'external-dependency/package.json');
  const mainExternal = path.join(workspaceRoot, 'external/dependency/local.ts');
  const external = path.join(root, 'external-dependency/main.ts');
  const externalValue = path.join(bazelBin, 'external/dependency/value.ts');
  const unscopedExternal = path.join(root, 'unscoped-external/main.ts');
  write(target, 'export const value = "declared generated module";\n');
  fs.mkdirSync(path.dirname(original), { recursive: true });
  fs.symlinkSync(target, original);
  write(checkout, 'export const value = "poisoned checkout";\n');
  write(authored, 'export { value } from "#value";\n');
  write(
    path.join(path.dirname(authored), 'package.json'),
    JSON.stringify({ imports: { '#value': './wrong.ts', '#npm': 'wrong-package' } }),
  );
  write(
    manifest,
    '\uFEFF' +
      JSON.stringify({
        name: '@fixture/scoped',
        exports: {
          './value': {
            browser: { development: './value.js', default: './wrong.ts' },
            default: './wrong.ts',
          },
        },
        imports: {
          '#value': {
            browser: { development: './value.js', default: './wrong.ts' },
            default: './wrong.ts',
          },
          '#npm': 'leftpad',
        },
      }),
  );
  write(
    mainScope,
    JSON.stringify({
      name: '@fixture/main',
      exports: { './value': './package-scope/value.js' },
      imports: { '#value': './package-scope/value.js', '#npm': 'leftpad' },
    }),
  );
  write(externalScope, JSON.stringify({
    name: '@fixture/external',
    exports: { './value': './value.js' },
    imports: { '#value': './value.js', '#npm': 'leftpad' },
  }));
  for (const source of [mainExternal, external, unscopedExternal]) {
    write(source, 'export { value } from "#value";\n');
  }
  write(externalValue, 'export const value = "external generated module";\n');
  const plugin = bazelPlugin({
    workspaceRoot,
    bazelBin,
    mode: 'serve',
    declaredFiles: {
      [logical]: { path: original, context: 'source', scope: manifest, importer: checkout },
      'package-scope/nested/main.ts': { path: authored, context: 'source', scope: manifest },
      'package-scope/package.json': { path: manifest, context: 'source' },
      'package.json': { path: mainScope, context: 'source' },
      'external/dependency/local.ts': { path: mainExternal, context: 'source', scope: mainScope },
      'external/dependency/main.ts': { path: external, context: 'source', scope: externalScope },
      'external/dependency/package.json': { path: externalScope, context: 'source' },
      'external/dependency/value.ts': { path: externalValue, context: 'source' },
      'external/unscoped/main.ts': { path: unscopedExternal, context: 'source' },
    },
  });
  plugin.configResolved({ root: workspaceRoot, logger: { info() {} } });
  const context = {
    environment: {
      config: {
        resolve: { conditions: ['browser', 'development|production'] },
        isProduction: false,
      },
    },
    addWatchFile() {},
    async resolve(id) {
      assert.ok(
        id === original || id === externalValue || id === 'leftpad',
        'selection must precede the filesystem resolver',
      );
      return { id, moduleSideEffects: false, meta: { selected: true } };
    },
  };
  const options = { isEntry: false, attributes: {}, custom: {} };
  for (const [importer, selected, self] of [
    [original, original, '@fixture/scoped'],
    [fs.realpathSync(original), original, '@fixture/scoped'],
    [authored, original, '@fixture/scoped'],
    [mainExternal, original, '@fixture/main'],
    [external, externalValue, '@fixture/external'],
  ]) {
    for (const specifier of ['#value?raw', self + '/value?raw']) {
      const result = await plugin.resolveId.handler.call(context, specifier, importer, options);
      assert.deepEqual(result, {
        id: selected + '?raw',
        moduleSideEffects: false,
        meta: { selected: true },
      });
    }
    assert.deepEqual(await plugin.resolveId.handler.call(context, '#npm', importer, options), {
      id: 'leftpad',
      moduleSideEffects: false,
      meta: { selected: true },
    });
  }
  assert.equal(
    await plugin.resolveId.handler.call(context, '#value', unscopedExternal, options),
    null,
  );
  assert.equal(
    await plugin.resolveId.handler.call(context, 'ordinary-package', original, options),
    null,
  );
});

test('declared generated modules and JSON cannot be replaced by checkout files', async () => {
  const extensions = ['.mjs', '.js', '.mts', '.ts', '.jsx', '.tsx', '.json'];
  for (const extension of ['tsx', 'js', 'mjs']) {
    const logical = `shared/extension-${extension}`;
    const generated = path.join(bazelBin, logical + '.' + extension);
    const shadow = path.join(workspaceRoot, logical + '.ts');
    write(generated, 'export const marker = "declared extension";\n');
    write(shadow, 'export const marker = "undeclared extension shadow";\n');
    const declaredFiles = { [logical + '.' + extension]: { path: generated, context: 'source' } };
    const resolver = new BazelResolver({ workspaceRoot, bazelBin, mode: 'serve', declaredFiles });
    const plugin = bazelPlugin({ workspaceRoot, bazelBin, mode: 'serve', declaredFiles });
    const context = { environment: { config: {
      root: path.join(workspaceRoot, 'shared'), resolve: { extensions },
    } } };
    plugin.configResolved({ root: context.environment.config.root, logger: { info() {} } });
    const requests = [
      '/extension-' + extension,
      '/extension-' + extension + '.' + extension,
      path.join(workspaceRoot, logical),
      generated,
      '/@fs/' + generated,
      '/@fs' + generated,
    ];
    if (extension === 'tsx') {
      write(path.join(workspaceRoot, logical + '.jsx'), 'export const marker = "checkout JSX";\n');
      requests.push('/extension-tsx.jsx', path.join(workspaceRoot, logical + '.jsx'));
      assert.equal(resolver.resolveId('../shared/extension-tsx.jsx', importers.serve)?.filePath, generated,
        'an explicit JSX specifier missed its declared TSX source');
    }
    for (const request of requests) {
      assert.equal(plugin.resolveId.handler.call(context, request + '?raw&path=/../kept#fragment', undefined),
        generated + '?raw&path=/../kept#fragment', 'URL normalization lost a declared scalar or its suffix');
    }
    const alias = { find: '@fixture/generated', replacement: path.join(workspaceRoot, 'shared') };
    for (const specifier of ['../shared/extension-' + extension,
      ('@fixture/generated/extension-' + extension).replace(alias.find, alias.replacement)]) {
      assert.equal(resolver.resolveId(specifier + '?raw', importers.serve, extensions)?.filePath,
        generated + '?raw', 'an earlier checkout extension replaced a declared scalar');
    }
    fs.unlinkSync(generated);
    assert.equal(resolver.resolveId('../shared/extension-' + extension, importers.serve, extensions)?.filePath,
      generated, 'a missing declared extension fell back to checkout');
    for (const request of requests) {
      assert.equal(plugin.resolveId.handler.call(context, request, undefined), generated,
        'a missing URL-selected scalar fell back to checkout');
    }
  }

  const alternatives = Object.fromEntries(['.js', '.mjs', '.ts', '.tsx'].map((extension) => {
    const logical = 'shared/ordered' + extension;
    const selected = path.join(bazelBin, logical);
    write(selected, 'export const marker = "' + extension + '";\n');
    return [logical, { path: selected, context: 'source' }];
  }));
  const ordered = bazelPlugin({ workspaceRoot, bazelBin, mode: 'serve', declaredFiles: alternatives });
  ordered.configResolved({ root: workspaceRoot, logger: { info() {} } });
  assert.equal(ordered.resolveId.handler.call({}, '../shared/ordered.jsx', importers.serve),
    path.join(bazelBin, 'shared/ordered.ts'), 'JSX aliases must preserve the native TS-before-TSX order');
  for (const order of [['.js', '.mjs', '.tsx'], ['.mjs', '.js', '.tsx']]) {
    const context = { environment: { config: { resolve: { extensions: order } } } };
    assert.equal(ordered.resolveId.handler.call(context, '../shared/ordered', importers.serve),
      path.join(bazelBin, 'shared/ordered' + order[0]), 'declared candidates ignored the native extension order');
  }
  assert.equal(ordered.resolveId.handler.call({ environment: { config: { resolve: { extensions: [] } } } },
    '../shared/ordered', importers.serve), null,
    'disabled native extensions must not use a second extension list');

  const generated = path.join(bazelBin, 'shared/generated.ts');
  const generatedJavaScript = path.join(bazelBin, 'shared/generated-js.js');
  const payload = path.join(bazelBin, 'shared/payload.json');
  const helper = path.join(workspaceRoot, 'shared/helper.ts');
  const firstTarget = path.join(root, 'codegen-first/value.ts');
  write(firstTarget, 'import { help } from "./helper.js";\nimport payload from "./payload.json";\nexport const value = [help, payload];\n');
  write(payload, '{"origin":"declared generated JSON"}\n');
  fs.symlinkSync(firstTarget, generated);
  write(helper, 'export const help = "authored sibling";\n');
  write(path.join(workspaceRoot, 'shared/generated.ts'), 'export const value = "stale checkout";\n');
  write(generatedJavaScript, 'export const value = "declared generated JavaScript";\n');
  write(path.join(workspaceRoot, 'shared/generated-js.ts'), 'export const value = "unrelated checkout TypeScript";\n');
  write(path.join(workspaceRoot, 'shared/payload.json'), '{"origin":"stale checkout"}\n');
  write(path.join(bazelBin, 'app/shared/generated.js'), 'export const value = "moved emitted module";\n');
  write(path.join(bazelBin, 'app/shared/generated.ts'), 'export const value = "moved source-mode module";\n');
  write(path.join(bazelBin, 'app/shared/payload.json'), '{"origin":"moved runtime JSON"}\n');

  const resolver = new BazelResolver({
    workspaceRoot, bazelBin, mode: 'serve',
    declaredFiles: {
      'shared/generated.ts': { path: generated, context: 'source' },
      'shared/generated-js.js': { path: generatedJavaScript, context: 'source' },
      'shared/payload.json': { path: payload, context: 'source' },
    },
  });

  for (const specifier of ['../shared/generated.ts', '../shared/generated.js']) {
    assert.deepEqual(resolver.resolveId(specifier, importers.serve), {
      filePath: generated, precompiled: false, mapPath: null,
    }, 'declared generated TypeScript was replaced by checkout source');
  }
  const selectedJavaScript = resolver.resolveId('../shared/generated-js.js', importers.serve);
  assert.deepEqual(selectedJavaScript, { filePath: generatedJavaScript, precompiled: true, mapPath: null });
  assert.equal(fs.readFileSync(selectedJavaScript.filePath, 'utf8'),
    'export const value = "declared generated JavaScript";\n');
  const selected = resolver.resolveId('../shared/payload.json', importers.serve);
  assert.deepEqual(selected, { filePath: payload, precompiled: false, mapPath: null });
  assert.deepEqual(JSON.parse(fs.readFileSync(selected.filePath, 'utf8')), {
    origin: 'declared generated JSON',
  });
  assert.deepEqual(resolver.resolveId('./helper.js', generated), {
    filePath: helper, precompiled: false, mapPath: null,
  }, 'a generated module lost its original source-relative import context');
  assert.deepEqual(resolver.resolveId('./payload.json', generated), selected);

  const linkedBin = path.join(root, 'declared-bin');
  const linkedWorkspace = path.join(root, 'declared-workspace');
  const canonicalHelper = path.join(linkedWorkspace, 'shared/helper.ts');
  fs.symlinkSync(bazelBin, linkedBin, 'dir');
  fs.symlinkSync(workspaceRoot, linkedWorkspace, 'dir');
  const canonical = new BazelResolver({
    workspaceRoot: linkedWorkspace, bazelBin: linkedBin, mode: 'serve',
    declaredFiles: {
      'shared/generated.ts': { path: path.join(linkedBin, 'shared/generated.ts'), context: 'source' },
      'shared/payload.json': { path: path.join(linkedBin, 'shared/payload.json'), context: 'source' },
      'shared/helper.ts': { path: canonicalHelper, context: 'source' },
    },
  });
  assert.deepEqual(canonical.resolveId('./helper.js', fs.realpathSync(generated)), {
    filePath: canonicalHelper, precompiled: false, mapPath: null,
  });
  assert.equal(canonical.resolveId('./payload.json', fs.realpathSync(generated))?.filePath,
    path.join(linkedBin, 'shared/payload.json'));
  assert.equal(canonical.isDeclaredFile(fs.realpathSync(payload)), true);
  assert.equal(canonical.isDeclaredFile(fs.realpathSync(helper)), true);
  assert.equal(canonical.resolveId('./payload.json', fs.realpathSync(helper))?.filePath,
    path.join(linkedBin, 'shared/payload.json'));
  assert.equal(canonical.resolveId('./helper.js', fs.realpathSync(payload))?.filePath,
    canonicalHelper);

  const canonicalPlugin = bazelPlugin({
    workspaceRoot: linkedWorkspace, bazelBin: linkedBin, mode: 'serve',
    declaredFiles: { 'shared/generated.ts': { path: path.join(linkedBin, 'shared/generated.ts'), context: 'source' } },
  });
  canonicalPlugin.configResolved({ root: path.join(linkedWorkspace, 'shared'), logger: { info() {} } });
  const canonicalContext = { environment: { config: {
    root: path.join(linkedWorkspace, 'shared'), resolve: { extensions },
  } } };
  for (const request of [firstTarget, '/@fs/' + firstTarget, path.join(workspaceRoot, 'shared/generated.ts')]) {
    assert.equal(canonicalPlugin.resolveId.handler.call(canonicalContext, request, undefined),
      path.join(linkedBin, 'shared/generated.ts'), 'a filesystem identity was reinterpreted as a browser URL');
  }
  for (const request of ['\0virtual', 'virtual:module', '/virtual:module', 'https://example.test/module', '//example.test/module', 'ordinary-package']) {
    assert.equal(canonicalPlugin.resolveId.handler.call(canonicalContext, request, undefined), null);
  }

  const missingScalar = path.join(bazelBin, 'shared/missing.ts');
  const missingOutput = path.join(bazelBin, 'shared/missing.js');
  const boundary = bazelPlugin({
    bazelBin, mode: 'serve', hmr: false,
    declaredFiles: {
      'shared/generated.ts': { path: generated, context: 'source' },
      'shared/missing.ts': { path: missingScalar, context: 'source' },
      'shared/missing.js': { path: missingOutput, context: 'source' },
    },
  });
  boundary.configResolved({ root: workspaceRoot, logger: { info() {} } });
  assert.equal(boundary.load(generated), null, 'an existing declared input lost native loading');
  for (const file of [missingScalar, missingOutput]) {
    assert.throws(() => boundary.load(file), { code: 'ENOENT' }, 'a declared miss retained Vite static-fallback permission');
  }
  for (const file of [path.join(workspaceRoot, 'undeclared.ts'), path.join(bazelBin, 'undeclared.js'), missingOutput + '?raw']) {
    assert.equal(boundary.load(file), null, 'an undeclared or query request left native loading');
  }

  const nextTarget = path.join(root, 'codegen-next/value.ts');
  write(nextTarget, 'export { help } from "./helper.js";\n');
  fs.symlinkSync(nextTarget, generated + '.replacement');
  fs.renameSync(generated + '.replacement', generated);
  assert.equal(canonical.isDeclaredFile(firstTarget), false, 'a replaced target is no longer declared');
  assert.equal(canonical.isDeclaredFile(nextTarget), true, 'the current generated target remains declared');
  assert.equal(canonical.resolveId('./helper.js', firstTarget), null);
  assert.deepEqual(canonical.resolveId('./helper.js', fs.realpathSync(generated)), {
    filePath: canonicalHelper, precompiled: false, mapPath: null,
  }, 'a replacement generated target lost its original source-relative import context');
  assert.equal(canonical.resolveId('./payload.json', fs.realpathSync(generated))?.filePath,
    path.join(linkedBin, 'shared/payload.json'));

  for (const [specifier, file] of [
    ['../shared/generated.js', generated],
    ['../shared/payload.json', payload],
  ]) {
    fs.unlinkSync(file);
    const missing = resolver.resolveId(specifier, importers.serve);
    assert.equal(missing?.filePath, file, 'missing declared inputs must not fall back to checkout');
    assert.throws(() => fs.readFileSync(missing.filePath), { code: 'ENOENT' });
  }
});

test('authored directory entries retain native Vite precedence and declared producer identity', async () => {
  const project = path.join(workspaceRoot, 'directory-entries');
  const importer = path.join(project, 'main.ts');
  const manifest = path.join(project, 'package.json');
  write(manifest, JSON.stringify({ imports: { '#directory': './generated-entry' } }));
  const declaredFiles = {
    'directory-entries/main.ts': { path: importer, context: 'source', isSource: true, scope: manifest },
    'directory-entries/package.json': { path: manifest, context: 'source', isSource: true },
  };
  const aliases = [];
  const cases = [];
  write(importer, 'export {};\n');
  for (const generated of [false, true]) {
    const name = generated ? 'generated-entry' : 'authored-entry';
    const directory = path.join(project, name);
    const client = path.join(directory, 'client.js');
    const index = path.join(directory, 'index.ts');
    const selected = generated ? path.join(bazelBin, 'directory-entries', name, 'client.js') : client;
    write(path.join(directory, 'package.json'), JSON.stringify({ main: './client.js' }));
    write(client, 'export const marker = "native package entry";\n');
    write(index, 'export const marker = "incorrect index entry";\n');
    if (generated) write(selected, 'export const marker = "declared package entry";\n');
    declaredFiles[`directory-entries/${name}/index.ts`] = { path: index, context: 'source', isSource: true };
    declaredFiles[`directory-entries/${name}/client.js`] = { path: selected, context: 'source', isSource: !generated };
    aliases.push({ find: '@fixture/' + name, replacement: directory });
    cases.push({ name, client, selected });
  }
  const generatedIndex = path.join(bazelBin, 'directory-entries/generated-index/index.ts');
  write(generatedIndex, 'export const marker = "declared index entry";\n');
  fs.mkdirSync(path.join(project, 'generated-index'));
  declaredFiles['directory-entries/generated-index/index.ts'] = { path: generatedIndex, context: 'source' };
  aliases.push({ find: '@fixture/generated-index', replacement: path.join(project, 'generated-index') });

  const servers = [];
  try {
    for (const plugins of [[], [bazelPlugin({ workspaceRoot, bazelBin, declaredFiles, mode: 'serve', hmr: false })]]) {
      servers.push(await createServer({
        root: project,
        configFile: false,
        envFile: false,
        logLevel: 'silent',
        appType: 'custom',
        server: { middlewareMode: true, watch: null, ws: false },
        optimizeDeps: { noDiscovery: true, include: [] },
        resolve: { alias: aliases },
        plugins,
      }));
    }
    const [native, plugin] = servers.map((server) => server.environments.client.pluginContainer);
    for (const { name, client, selected } of cases) {
      const requests = ['@fixture/' + name, './' + name];
      if (name === 'generated-entry') requests.push('#directory');
      for (const request of requests) {
        for (const suffix of ['', '?raw&path=/../kept#fragment']) {
          const nativeResult = await native.resolveId(request + suffix, importer);
          const result = await plugin.resolveId(request + suffix, importer);
          // Native Vite leaves an aliased directory with a query unresolved.
          if (suffix !== '' && request.startsWith('@fixture/')) {
            assert.equal(result?.id, nativeResult?.id, request + ' changed native alias resolution');
            continue;
          }
          // The plugin container posix-normalizes every resolved id, query included.
          assert.equal(nativeResult?.id, path.posix.normalize(fs.realpathSync(client) + suffix), 'native Vite did not select the package main');
          assert.equal(result?.id, path.posix.normalize(selected + suffix), request + ' replaced native package main with the declared index');
        }
      }
    }
    assert.equal(await native.resolveId('./generated-index', importer), null);
    for (const request of ['@fixture/generated-index', './generated-index']) {
      assert.equal((await plugin.resolveId(request, importer))?.id, generatedIndex,
        'native directory delegation lost the declared generated scalar index');
    }
    fs.unlinkSync(generatedIndex);
    write(path.join(project, 'generated-index/index.js'), 'export const marker = "checkout index shadow";\n');
    assert.equal((await plugin.resolveId('./generated-index', importer))?.id, generatedIndex,
      'a missing declared scalar index was replaced by checkout JavaScript');
  } finally {
    await Promise.all(servers.map((server) => server.close()));
  }
});

test('a declared generated tree cannot be replaced by checkout files', async () => {
  const tree = path.join(bazelBin, 'app/compiled');
  const entry = path.join(tree, 'index.js');
  const child = path.join(tree, 'messages/greeting.js');
  const checkout = path.join(workspaceRoot, 'app/compiled');
  const firstTree = path.join(root, 'compiled-first');
  fs.mkdirSync(firstTree);
  fs.symlinkSync(firstTree, tree, 'dir');
  write(entry, 'export { greeting } from "./messages/greeting.js";\n');
  write(path.join(tree, 'index.d.ts'), 'export { greeting } from "./messages/greeting.js";\n');
  write(child, 'export const greeting = "declared tree";\n');
  write(tree + '.js', 'export const greeting = "outside declared tree";\n');
  write(path.join(checkout, 'index.ts'), 'export const greeting = "stale checkout TypeScript";\n');
  write(path.join(checkout, 'index.js'), 'export const greeting = "stale checkout JavaScript";\n');
  write(path.join(checkout, 'messages/greeting.ts'), 'export const greeting = "stale checkout child";\n');
  const resolver = new BazelResolver({
    workspaceRoot, bazelBin, mode: 'serve',
    declaredFiles: {
      'app/compiled': { path: tree, context: 'source', directory: true },
    },
  });

  assert.deepEqual(resolver.resolveId('./compiled/index.js', importers.serve), {
    filePath: entry, precompiled: true, mapPath: null,
  }, 'declared generated tree member was replaced by checkout TypeScript');
  for (const specifier of ['./compiled', './compiled/index', './compiled/messages/greeting']) {
    assert.equal(resolver.resolveId(specifier, importers.serve)?.filePath,
      specifier === './compiled' ? tree + path.sep : path.join(bazelBin, 'app', specifier),
      'directory and extension resolution must stay in the declared namespace');
  }
  const plugin = bazelPlugin({
    workspaceRoot, bazelBin, mode: 'serve',
    declaredFiles: { 'app/compiled': { path: tree, context: 'source', directory: true } },
  });
  plugin.configResolved({ root: workspaceRoot, logger: { info() {} } });
  const options = { isEntry: false, attributes: {}, custom: {} };
  const metadata = { moduleSideEffects: false, meta: { selected: true } };
  const context = {
    async resolve(id, importer, forwarded) {
      assert.equal(id, tree + path.sep);
      assert.equal(importer, importers.serve);
      assert.deepEqual(forwarded, { ...options, skipSelf: true });
      return { id: fs.realpathSync(child), ...metadata };
    },
  };
  assert.deepEqual(await plugin.resolveId.handler.call(context, './compiled?raw', importers.serve, options),
    { id: child + '?raw', ...metadata });
  assert.deepEqual(await plugin.resolveId.handler.call({
    ...context,
    environment: { config: { root: path.join(workspaceRoot, 'app'), resolve: { extensions: [] } } },
  }, '/compiled?raw', importers.serve, options), { id: child + '?raw', ...metadata },
  'a browser-root tree must use the same declared-directory resolution');
  for (const escaped of [
    { id: tree + '.js' },
    { id: path.join(checkout, 'index.js') },
    { id: '\0generated-package' },
    { id: 'external-package', external: true },
  ]) {
    await assert.rejects(plugin.resolveId.handler.call({
      resolve: async () => escaped,
    }, './compiled', importers.serve, options), /outside declared directory/);
  }
  fs.unlinkSync(path.join(checkout, 'index.ts'));
  assert.equal(resolver.resolveId('./compiled/index.js', importers.serve)?.filePath, entry,
    'declared generated tree member was delegated to checkout JavaScript');
  assert.deepEqual(resolver.resolveId('./messages/greeting.js', entry), {
    filePath: child, precompiled: true, mapPath: null,
  }, 'a tree member imported a stale checkout sibling');

  const outside = path.join(workspaceRoot, 'app/compiled-other.ts');
  write(outside, 'export const outside = "authored sibling";\n');
  assert.equal(resolver.resolveId('./compiled-other.js', importers.serve)?.filePath, outside,
    'tree authority must end at its directory boundary');
  assert.equal(resolver.resolveId(tree + '/../compiled-other.ts')?.filePath, outside);
  assert.equal(resolver.resolveId('../compiled-other.js', entry)?.filePath, outside,
    'a generated tree importer lost its original source-relative coordinate');

  const firstImporter = fs.realpathSync(entry);
  assert.equal(resolver.resolveId('./messages/greeting.js', firstImporter)?.filePath, child);
  const nextTree = path.join(root, 'compiled-next');
  write(path.join(nextTree, 'index.js'), 'export { greeting } from "./messages/greeting.js";\n');
  write(path.join(nextTree, 'messages/greeting.js'), 'export const greeting = "replacement tree";\n');
  fs.symlinkSync(nextTree, tree + '.replacement', 'dir');
  fs.renameSync(tree + '.replacement', tree);
  assert.equal(resolver.isDeclaredFile(firstImporter), false, 'the replaced canonical tree is no longer declared');
  assert.equal(resolver.resolveId('./messages/greeting.js', firstImporter), null);
  assert.equal(resolver.isDeclaredFile(fs.realpathSync(entry)), true);
  assert.equal(resolver.resolveId('./messages/greeting.js', fs.realpathSync(entry))?.filePath, child,
    'the replacement canonical tree lost its declared coordinate');

  fs.unlinkSync(entry);
  const missing = resolver.resolveId('./compiled/index.js', importers.serve);
  assert.equal(missing?.filePath, entry, 'a missing tree member must not fall back to checkout');
  assert.throws(() => fs.readFileSync(missing.filePath), { code: 'ENOENT' });
  assert.equal(await plugin.resolveId.handler.call({ resolve: async () => null },
    './compiled/index.js', importers.serve, options), entry,
    'a native resolution miss must retain the declared producer path');
});

let failed = 0;
for (const [name, fn] of tests) {
  try {
    await fn();
    process.stdout.write(`PASS: ${name}\n`);
  } catch (err) {
    failed++;
    process.stdout.write(`FAIL: ${name}\n${err && err.stack ? err.stack : err}\n`);
  }
}

process.stdout.write(`\n${tests.length - failed}/${tests.length} passed\n`);
process.exit(failed === 0 ? 0 : 1);
