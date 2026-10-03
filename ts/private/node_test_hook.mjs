import { readFileSync, statSync } from "node:fs";
import module from "node:module";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

if (typeof module.registerHooks !== "function") {
  throw new Error(
    "ts_test //ts/runners:node_test: node:module.registerHooks is " +
      `unavailable on Node ${process.version}, so the package's code cannot ` +
      "resolve at its runfiles paths. Upgrade the js_runtime toolchain.",
  );
}

// ESM ignores NODE_PATH; the launcher's declared chain supplies absent native packages.
const trees = (process.env.NODE_PATH ?? "")
  .split(path.delimiter)
  .filter(Boolean)
  .map((dir) => ({
    parentURL: pathToFileURL(path.dirname(dir) + path.sep).href,
  }));

const COMPILED = {
  ".ts": [".js"],
  ".tsx": [".js", ".jsx"],
  ".mts": [".mjs"],
  ".cts": [".cjs"],
};

// The compiled files a `.ts` specifier names, relative or bare: a subpath
// into a workspace member's view holds them too.
function compiledExtensionForms(specifier) {
  const ext = path.extname(specifier);
  if (!(ext in COMPILED)) return [];
  return COMPILED[ext].map((c) => specifier.slice(0, -ext.length) + c);
}

// The files the compiled tree holds for a specifier tsc resolved under the
// tsconfig: a .ts's sibling; the .js or index.js of an extensionless one.
function compiledForms(specifier) {
  const forms = compiledExtensionForms(specifier);
  if (forms.length > 0 || path.extname(specifier) !== "") return forms;
  return [`${specifier}.js`, `${specifier}/index.js`];
}

function firstResolved(forms, attempt) {
  for (const form of forms) {
    try {
      return attempt(form);
    } catch {
      // The specifier as written decides.
    }
  }
  return undefined;
}

function isFile(url) {
  return statSync(url, { throwIfNoEntry: false })?.isFile() ?? false;
}

// A store file's realpath and a member's store path hold the segment; a test
// file's runfiles path does not.
function insideATree(parentURL) {
  const segment = `${path.sep}node_modules${path.sep}`;
  return fileURLToPath(parentURL).includes(segment);
}

function isBare(specifier) {
  return (
    !specifier.startsWith("/") &&
    !specifier.startsWith("#") &&
    !module.isBuiltin(specifier) &&
    !URL.canParse(specifier)
  );
}

function packageScope(parentURL) {
  let dir = path.dirname(fileURLToPath(parentURL));
  for (;;) {
    const manifest = path.join(dir, "package.json");
    if (isFile(pathToFileURL(manifest))) {
      const { name, exports } = JSON.parse(readFileSync(manifest, "utf8"));
      return { dir, name, exports };
    }
    const parent = path.dirname(dir);
    if (parent === dir) return null;
    dir = parent;
  }
}

// node's own first step for a bare specifier: the scope names the
// specifier's package and has `exports`.
function isSelfReference(specifier, scope) {
  return (
    scope !== null &&
    scope.exports != null &&
    (specifier === scope.name || specifier.startsWith(`${scope.name}/`))
  );
}

// The manifest as written names a source; the compiled sibling at the source's
// runfiles path runs.
function compiledSibling(resolved, scope, asWritten) {
  if (!resolved.url.startsWith("file:") || scope === null) return resolved;
  const resolvedURL = new URL(resolved.url);
  const held = fileURLToPath(resolvedURL);
  if (!held.startsWith(scope.dir + path.sep)) return resolved;
  for (const form of compiledExtensionForms(held)) {
    const url = pathToFileURL(form);
    if (isFile(url)) {
      url.search = resolvedURL.search;
      url.hash = resolvedURL.hash;
      return { ...resolved, ...asWritten(url.href) };
    }
  }
  return resolved;
}

function resolvePackage(specifier, context, next) {
  let notFound;
  for (const { parentURL } of [context, ...trees]) {
    try {
      return next(specifier, { ...context, parentURL });
    } catch (error) {
      if (error?.code !== "ERR_MODULE_NOT_FOUND") throw error;
      try {
        module.findPackageJSON(specifier, parentURL);
      } catch (lookupError) {
        if (lookupError?.code === "ERR_MODULE_NOT_FOUND") {
          notFound ??= error;
          continue;
        }
        throw lookupError;
      }
      throw error;
    }
  }
  throw notFound;
}

function resolve(specifier, sharedContext, next) {
  // Node merges next() overrides into the context object supplied to this hook.
  const context = { ...sharedContext };
  const { parentURL } = context;
  // Node leaves importAttributes undefined for require, including under custom conditions.
  const imported = context.importAttributes !== undefined;
  const asWritten = (form) => next(form, context);
  if (!parentURL?.startsWith("file:") || insideATree(parentURL)) {
    return firstResolved(compiledExtensionForms(specifier), asWritten) ?? asWritten(specifier);
  }
  if (specifier.startsWith("./") || specifier.startsWith("../")) {
    for (const form of [...compiledForms(specifier), specifier]) {
      const url = new URL(form, parentURL);
      if (isFile(url)) return asWritten(form);
    }
    return asWritten(specifier);
  }
  const privateImport = specifier.startsWith("#");
  const scope = privateImport || isBare(specifier) ? packageScope(parentURL) : null;
  if (privateImport || isSelfReference(specifier, scope)) {
    return compiledSibling(asWritten(specifier), scope, asWritten);
  }
  // require() reads NODE_PATH itself and ignores a swapped parentURL.
  if (trees.length > 0 && isBare(specifier)) {
    const attempt = imported ? (form) => resolvePackage(form, context, next) : asWritten;
    return firstResolved(compiledExtensionForms(specifier), attempt) ?? attempt(specifier);
  }
  return asWritten(specifier);
}

module.registerHooks({ resolve });
