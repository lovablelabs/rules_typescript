"""Source-built oj dev servers, with an optional Vite backend."""

load(
    "//tools/launcher:launcher.bzl",
    "LAUNCHER_TOOLCHAINS",
    "declare_launcher",
    "rlocation_path",
)
load("//ts/private:node_modules.bzl", "check_dev_server_npm_contexts")
load("//ts/private:providers.bzl", "DevServerInfo", "NodeModulesInfo", "TsInfo", "canonical_runtime_file", "runtime_links", "runtime_scope_destinations")
load("//ts/private:runtime.bzl", "JS_RUNTIME_TOOLCHAIN_TYPE", "get_js_runtime")
load("//ts/private:vite_config.bzl", "LOAD_USER_CONFIG_JS", "VITE_CONFIG_EXTENSIONS", "VITE_CONFIG_SRCS_DOC", "stage_vite_config")

def _logical_path(f):
    if f.short_path.startswith("../"):
        return "external/" + f.short_path[3:]
    return f.short_path

def _file_path_js(ctx, source):
    if source.is_source and not source.short_path.startswith("../"):
        return "path.resolve(workspaceRoot, {})".format(json.encode(source.short_path))
    if source.path.startswith(ctx.bin_dir.path + "/"):
        relative = source.path[len(ctx.bin_dir.path) + 1:]
    else:
        relative = "../" * len(ctx.bin_dir.path.split("/")) + source.path
    return "path.resolve(fs.realpathSync(bazelBin), {})".format(json.encode(relative))

def _add_declared_file(files, logical, original, selected, context):
    previous = files.get(logical)
    if previous != None and previous[0] != original:
        fail(("ts_dev_server: input '{}' has distinct source Files '{}' and '{}'. " +
              "Did you mean to give these inputs separate logical paths?").format(logical, previous[0].path, original.path))
    files[logical] = (original, selected, context)

def _server_config_input_js(server_info, server_binary_rl):
    """The restart input naming whichever dev server is actually serving.

    A rebuild that moved the server -- a new Vite in the npm tree, a new native
    server binary -- leaves a running server that is no longer the one the graph
    was planned around. Which file that is depends on the implementation, so it
    cannot be the hardcoded `vite/package.json` it used to be: a native server
    need not have vite in its tree at all.
    """
    if server_info.server_in_tree:
        segments = server_info.server_in_tree.split("/")
        pkg = "/".join(segments[0:2]) if server_info.server_in_tree.startswith("@") else segments[0]
        return (
            "if (nodeModulesPath) {\n" +
            "  configInputs.push({\n" +
            "    label: " + json.encode("{} in the Bazel npm tree".format(pkg)) + ",\n" +
            "    path: path.join(nodeModulesPath, " + json.encode(pkg) + ", 'package.json'),\n" +
            "    digest: 'content',\n" +
            "    remedy: 'manual',\n" +
            "  });\n" +
            "}\n"
        )
    return (
        "if (process.env['RUNFILES_DIR']) {\n" +
        "  configInputs.push({\n" +
        "    label: 'the dev server binary',\n" +
        "    path: path.join(process.env['RUNFILES_DIR'], " + json.encode(server_binary_rl) + "),\n" +
        "    digest: 'identity',\n" +
        "    remedy: 'manual',\n" +
        "  });\n" +
        "}\n"
    )

def _generate_dev_config(
        ctx,
        has_node_modules,
        plugin_rl,
        react_refresh,
        runtime_rl,
        server_input_js,
        declared_files,
        npm_contexts,
        npm_views,
        user_config_rl = ""):
    """Generates a vite.config.mjs for dev server mode.

    The config is designed to work in conjunction with the launcher:
      - BAZEL_BIN_DIR env var is set to the bazel-bin path.
      - BUILD_WORKSPACE_DIRECTORY is set by `bazel run`.
      - NODE_MODULES_PATH env var is set to the importer's node_modules
        directory.
      - VITE_PLUGIN_PATH env var is set to the compiled vite-plugin-bazel .mjs
        (only when the plugin attr is set).
      - VITE_USER_CONFIG_PATH env var is set to the user-supplied plugin config
        (only when the vite_config attr is set).

    Args:
        ctx: The rule context.
        has_node_modules: Whether an importer was declared.
        plugin_rl: Runfiles-tree-relative path to the compiled
            vite_plugin_bazel.mjs, or empty string if not set.
        react_refresh: bool, whether to import and use @vitejs/plugin-react
            for React Fast Refresh (HMR that preserves component state).
        runtime_rl: Runfiles-tree-relative path of the toolchain node binary,
            so the config can watch the one it is running under.
        server_input_js: The JavaScript, from _server_config_input_js, that adds
            the selected dev server to configInputs.
        user_config_rl: Runfiles-tree-relative path to the bin copy of the
            user-supplied Vite plugin config, or empty string if there is none.
            When set, the generated config dynamically imports it and prepends
            its plugins before the Bazel system plugins.

    Returns:
        The generated vite.config.mjs File.
    """
    scopes = [original for original, _selected, context in declared_files.values() if context == "source" and original.basename == "package.json"]
    declared_paths = []
    for logical, (original, selected, context) in sorted(declared_files.items()):
        source_scope = ""
        importer = ""
        if context == "source" and not original.is_directory:
            for scope in runtime_scope_destinations(scopes, original, original.path):
                source_scope = ", scope: " + _file_path_js(ctx, scope)
                if not original.is_source and scope.is_source:
                    relative = "/".join(original.short_path.split("/")[len(scope.short_path.split("/")) - 1:])
                    importer = ", importer: path.resolve(path.dirname({}), {})".format(_file_path_js(ctx, scope), json.encode(relative))
        declared_paths.append(
            "[{}]: {{ path: {}, context: {}, isSource: {}{}{}{} }}".format(json.encode(logical), _file_path_js(ctx, selected), json.encode(context), json.encode(selected.is_source), ", directory: true" if selected.is_directory else "", source_scope, importer),
        )

    npm_paths = [
        "[{}, {}]".format(json.encode(logical), json.encode(npm_contexts[original]))
        for logical, (original, _selected, _context) in sorted(declared_files.items())
        if original in npm_contexts
    ]

    port = ctx.attr.port
    host = ctx.attr.host
    open_browser = ctx.attr.open

    # Declared before the content is built: the config watches itself, and only
    # Bazel knows where the file it is about to write will land.
    config_file = ctx.actions.declare_file(
        "{}_dev/vite.config.mjs".format(ctx.label.name),
    )

    open_js = "true" if open_browser else "false"
    host_js = json.encode(host) if host else "true"

    config_content = (
        "// Generated by rules_typescript ts_dev_server for " + str(ctx.label) + "\n" +
        "// DO NOT EDIT — regenerated on every build.\n" +
        "//\n" +
        "// Environment variables read at startup:\n" +
        "//   BUILD_WORKSPACE_DIRECTORY — workspace root (set by `bazel run`)\n" +
        "//   BAZEL_BIN_DIR             — absolute path to the bazel-bin symlink\n" +
        "//   NODE_MODULES_PATH         — absolute path to the importer's\n" +
        "//                              node_modules\n" +
        (
            "//   VITE_PLUGIN_PATH           — absolute path to vite_plugin_bazel.mjs\n" if plugin_rl else ""
        ) +
        (
            "//   VITE_USER_CONFIG_PATH      — absolute path to the user-supplied plugin config\n" if user_config_rl else ""
        ) +
        "\n" +
        "import fs from 'node:fs';\n" +
        "import path from 'node:path';\n" +
        "\n" +
        "// Resolve key directories from environment variables.\n" +
        "// BUILD_WORKSPACE_DIRECTORY is set by `bazel run`; fall back to process.cwd().\n" +
        "const workspaceRoot = process.env['BUILD_WORKSPACE_DIRECTORY'] || process.cwd();\n" +
        "\n" +
        "// bazel-bin is typically a symlink at <workspace>/bazel-bin.\n" +
        "const bazelBin = process.env['BAZEL_BIN_DIR'] || path.join(workspaceRoot, 'bazel-bin');\n" +
        "\n" +
        "// The importer's node_modules directory, absolute, in runfiles.\n" +
        "const nodeModulesPath = process.env['NODE_MODULES_PATH'] || null;\n" +
        "\n" +
        "const declaredFiles = {" + ", ".join(declared_paths) + "};\n" +
        "const npmContexts = [" + ", ".join(npm_paths) + "];\n" +
        "const admittedNpmPaths = new Set();\n" +
        "const npmViews = " + json.encode(npm_views) + ".map(view => path.join(path.dirname(nodeModulesPath), view));\n" +
        "const memberNpmViews = new Map();\n" +
        "const slashPath = file => path.sep === '\\\\' ? file.replace(/\\\\/g, '/') : file;\n" +
        "const npmStores = new Map();\n" +
        "const realpath = file => {\n" +
        "  try { return fs.realpathSync(file); } catch (error) {\n" +
        "    if (error.code !== 'ENOENT' && error.code !== 'ENOTDIR') throw error;\n" +
        "  }\n" +
        "};\n" +
        "// A sandbox may rebuild a store directory from per-file links; the manifest's\n" +
        "// realpath names the same package either way.\n" +
        "const npmStore = directory => {\n" +
        "  const manifest = realpath(path.join(directory, 'package.json'));\n" +
        "  return manifest === undefined ? realpath(directory) : path.dirname(manifest);\n" +
        "};\n" +
        "for (const [logical, bindings] of npmContexts) {\n" +
        "  const file = declaredFiles[logical];\n" +
        "  const directories = new Set();\n" +
        "  for (const source of [file.path, file.importer]) {\n" +
        "    if (source === undefined) continue;\n" +
        "    directories.add(file.directory ? source : path.dirname(source));\n" +
        "    const resolved = realpath(source);\n" +
        "    if (resolved !== undefined) directories.add(file.directory ? resolved : path.dirname(resolved));\n" +
        "  }\n" +
        "  for (const directory of [...directories]) {\n" +
        "    const resolved = realpath(directory);\n" +
        "    if (resolved !== undefined) directories.add(resolved);\n" +
        "  }\n" +
        "  for (const [name, member] of Object.entries(bindings)) {\n" +
        "    const view = member === null ? nodeModulesPath : npmViews[member];\n" +
        "    const linked = path.join(view, name);\n" +
        "    if (!npmStores.has(linked)) npmStores.set(linked, npmStore(linked) ?? fs.realpathSync(linked));\n" +
        "    const expected = npmStores.get(linked);\n" +
        "    if (member !== null && path.basename(file.path) !== 'package.json') {\n" +
        "      for (const source of [file.path, file.importer, realpath(file.path)]) {\n" +
        "        if (source === undefined) continue;\n" +
        "        const key = slashPath(source);\n" +
        "        if (!memberNpmViews.has(key)) memberNpmViews.set(key, new Map());\n" +
        "        memberNpmViews.get(key).set(name, member);\n" +
        "      }\n" +
        "    }\n" +
        "    for (const source of directories) {\n" +
        "      for (let directory = source; ; directory = path.dirname(directory)) {\n" +
        "        if (path.basename(directory) !== 'node_modules') {\n" +
        "          const candidate = path.join(directory, 'node_modules', name);\n" +
        "          if (admittedNpmPaths.has(candidate + '\\0' + expected)) break;\n" +
        "          const actual = npmStore(candidate);\n" +
        "          if (actual !== undefined && actual !== expected) {\n" +
        "            throw new Error('[ts_dev_server] conflicting npm installation for ' + name + ' from ' + source +\n" +
        "              ': ' + candidate + ' resolves to ' + actual + ', but the declared ' + (member === null ? 'app' : 'member importer') + ' store is ' + expected +\n" +
        "              '. Remove the conflicting installation yourself or link this package to the declared store before restarting.');\n" +
        "          }\n" +
        "          admittedNpmPaths.add(candidate + '\\0' + expected);\n" +
        "          if (actual !== undefined) break;\n" +
        "        }\n" +
        "        if (path.dirname(directory) === directory) break;\n" +
        "      }\n" +
        "    }\n" +
        "  }\n" +
        "}\n" +
        "\n"
    )

    config_content += (
        "// The inputs this config was generated from. A rebuild that changes one of\n" +
        "// them leaves the running server configured for a graph that no longer\n" +
        "// exists; a rebuild that only rewrote ts_codegen output leaves it correct,\n" +
        "// and the bazel-bin watcher turns that into HMR instead of a restart.\n" +
        "const configInputs = [\n" +
        "  {\n" +
        "    label: 'the generated vite config',\n" +
        "    path: path.join(bazelBin, " + json.encode(_logical_path(config_file)) + "),\n" +
        "    digest: 'content',\n" +
        "    remedy: 'restart',\n" +
        "  },\n" +
        "];\n" +
        server_input_js +
        "// The runfiles symlink, not process.execPath: execPath is already resolved\n" +
        "// to the old toolchain's real file, which a new toolchain does not touch.\n" +
        "const runfilesDir = process.env['RUNFILES_DIR'];\n" +
        "if (runfilesDir) {\n" +
        "  configInputs.push({\n" +
        "    label: 'the toolchain node binary',\n" +
        "    path: path.join(runfilesDir, " + json.encode(runtime_rl) + "),\n" +
        "    digest: 'identity',\n" +
        "    remedy: 'manual',\n" +
        "  });\n" +
        "}\n" +
        "\n" +
        "export const bazelConfigInputs = configInputs;\n" +
        "\n"
    )

    # A static `import react from '@vitejs/plugin-react'` resolves from the config
    # file's own directory, which is a generated one with no node_modules above it.
    if react_refresh:
        react_load_failed = json.encode(
            "[ts_dev_server] {} sets react_refresh = True, but @vitejs/plugin-react did not ".format(ctx.label) +
            "load from the node_modules the dev server links. Add " +
            "@npm//:vitejs_plugin-react to the deps of the node_modules() " +
            "target this dev server uses. Cause: ",
        )
        config_content += (
            "// A package's own `exports` map is the only authority on its entry point;\n" +
            "// a path into its dist/ is a guess at a layout the package reorganises.\n" +
            "function npmExportsEntry(node) {\n" +
            "  if (typeof node === 'string') return node;\n" +
            "  if (node === null || typeof node !== 'object') return null;\n" +
            "  if (Array.isArray(node)) {\n" +
            "    for (const alternative of node) {\n" +
            "      const hit = npmExportsEntry(alternative);\n" +
            "      if (hit) return hit;\n" +
            "    }\n" +
            "    return null;\n" +
            "  }\n" +
            "  const keys = Object.keys(node);\n" +
            "  if (keys.some((key) => key.startsWith('.'))) return npmExportsEntry(node['.']);\n" +
            "  for (const condition of ['import', 'module', 'default']) {\n" +
            "    if (condition in node) {\n" +
            "      const hit = npmExportsEntry(node[condition]);\n" +
            "      if (hit) return hit;\n" +
            "    }\n" +
            "  }\n" +
            "  return null;\n" +
            "}\n" +
            "\n" +
            "function npmEntryPath(pkg) {\n" +
            "  const dir = path.join(nodeModulesPath, pkg);\n" +
            "  const manifest = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'));\n" +
            "  const entry = npmExportsEntry(manifest.exports) || manifest.module || manifest.main;\n" +
            "  if (!entry) throw new Error(pkg + ' in ' + dir + ' declares no entry point');\n" +
            "  return path.join(dir, entry);\n" +
            "}\n" +
            "\n" +
            "let react;\n" +
            "try {\n" +
            "  react = (await import(npmEntryPath('@vitejs/plugin-react'))).default;\n" +
            "} catch (err) {\n" +
            "  throw new Error(" + react_load_failed + " + err.message);\n" +
            "}\n" +
            "\n"
        )

    # Add plugin import when plugin is wired in.
    # We use a dynamic import pattern to load the plugin from the env-var path.
    # Since vite.config.mjs is evaluated as ESM, we can use a top-level await
    # or use createRequire for the dynamic load.
    # The simplest approach: conditionally use the plugin via dynamic import().
    if plugin_rl:
        config_content += (
            "// Load the vite-plugin-bazel from the runfiles path.\n" +
            "// The plugin path is passed via VITE_PLUGIN_PATH env var.\n" +
            "const pluginPath = process.env['VITE_PLUGIN_PATH'];\n" +
            "let bazelPluginFn = null;\n" +
            "if (pluginPath) {\n" +
            "  try {\n" +
            "    const mod = await import(pluginPath);\n" +
            "    bazelPluginFn = mod.bazelPlugin;\n" +
            "  } catch (err) {\n" +
            "    console.warn('[ts_dev_server] Failed to load vite-plugin-bazel:', err.message);\n" +
            "  }\n" +
            "}\n" +
            "\n"
        )

    if user_config_rl:
        config_content += LOAD_USER_CONFIG_JS

    config_content += (
        "// Build the list of directories Vite's dev server is allowed to serve.\n" +
        "const fsAllow = [workspaceRoot, bazelBin];\n" +
        "if (nodeModulesPath) fsAllow.push(nodeModulesPath);\n" +
        "fsAllow.push(...npmViews);\n" +
        "\n" +
        "// Vite matches a request against the resolved path, so an allow entry that\n" +
        "// is still a symlink never matches it: on macOS /var is /private/var, and\n" +
        "// bazel-bin is a symlink on every platform. Both forms are kept because\n" +
        "// which one a request arrives as depends on how the server was started.\n" +
        "for (const dir of [...fsAllow]) {\n" +
        "  try {\n" +
        "    const real = fs.realpathSync(dir);\n" +
        "    if (real !== dir) fsAllow.push(real);\n" +
        "  } catch {}\n" +
        "}\n" +
        "\n"
    )

    if has_node_modules:
        config_content += (
            "// A fallback, not the mechanism: the launcher links the\n" +
            "// importer's node_modules in as <workspace>/node_modules, so\n" +
            "// the walk up from an importer finds it the way it would\n" +
            "// outside Bazel. This catches what that walk cannot see --\n" +
            "// an importer outside the workspace, or a server whose\n" +
            "// resolver does no walk at all -- by handing the id back\n" +
            "// with an importer under the directory: the package's own\n" +
            "// manifest. Exports maps, conditions and subpaths stay the\n" +
            "// resolver's. It runs 'post' so it only fires where the\n" +
            "// primary resolver came back empty; at 'pre' it rewrote\n" +
            "// every bare importer into node_modules, which reads to Vite\n" +
            "// as a node_modules-internal import and opts the module out\n" +
            "// of optimisation.\n" +
            "const bazelNpmResolve = {\n" +
            "  name: 'bazel:npm-resolve',\n" +
            "  enforce: 'post',\n" +
            "  async resolveId(id, importer, options) {\n" +
            "    if (id.startsWith('.') || id.startsWith('/') || id.includes(':') || id.includes('\\0')) {\n" +
            "      return null;\n" +
            "    }\n" +
            "    const segments = id.split('/');\n" +
            "    const pkg = id.startsWith('@') ? segments.slice(0, 2).join('/') : segments[0];\n" +
            "    // A member's source resolves its own importer's links, not the app's.\n" +
            "    const member = !importer ? undefined : memberNpmViews.get(slashPath(importer.split('?')[0]))?.get(pkg);\n" +
            "    const view = member === undefined ? nodeModulesPath : npmViews[member];\n" +
            "    const manifest = path.join(view, pkg, 'package.json');\n" +
            "    if (!fs.existsSync(manifest)) return null;\n" +
            "    return this.resolve(id, manifest, { ...options, skipSelf: true });\n" +
            "  },\n" +
            "};\n" +
            "\n"
        )

    # User plugins first: a framework transform has to see a module before the
    # Bazel ones. The npm resolver last, so anything above it can claim an id.
    config_content += "const plugins = [{}];\n".format("..._userPlugins" if user_config_rl else "")
    if react_refresh:
        config_content += (
            "// React Fast Refresh — preserves component state across HMR updates.\n" +
            "plugins.push(react());\n"
        )
    if plugin_rl:
        config_content += (
            "if (bazelPluginFn) {\n" +
            "  plugins.push(bazelPluginFn({\n" +
            "    bazelBin: bazelBin,\n" +
            "    workspaceRoot: workspaceRoot,\n" +
            "    declaredFiles,\n" +
            "    nodeModules: nodeModulesPath || undefined,\n" +
            "    configInputs: bazelConfigInputs,\n" +
            "  }));\n" +
            "}\n"
        )
    if has_node_modules:
        config_content += "plugins.push(bazelNpmResolve);\n"
    config_content += "\n"

    config_content += (
        "// @type {import('vite').UserConfig}\n" +
        "export default {\n" +
        "  root: path.join(workspaceRoot, " + json.encode(ctx.label.package) + "),\n" +
        "\n" +
        "  server: {\n" +
        "    port: " + str(port) + ",\n" +
        "    host: " + host_js + ",\n" +
        "    open: " + open_js + ",\n" +
        "    fs: {\n" +
        "      allow: fsAllow,\n" +
        "    },\n" +
        "  },\n" +
        "\n" +
        "  // A bare specifier is left to the resolver's own walk up from\n" +
        "  // the importer, which the launcher's <workspace>/node_modules\n" +
        "  // link puts the links on; a workspace member is among them\n" +
        "  // through its link target. There is no resolve.modules: a\n" +
        "  // webpack option, and Vite ignores it.\n" +
        "  plugins,\n" +
        "\n"
    )

    config_content += (
        "  // node_modules/.vite is the default, and the launcher just pointed that\n" +
        "  // name at a read-only Bazel output. Pre-bundling is not optional here:\n" +
        "  // react and friends ship CJS, and the browser needs the ESM it writes.\n" +
        "  cacheDir: path.join(bazelBin, " + json.encode(_logical_path(config_file).rsplit("/", 1)[0] + "/vite-cache") + "),\n" +
        "\n" +
        "  publicDir: false,\n" +
        "\n" +
        "  logLevel: 'info',\n" +
        "};\n"
    )

    ctx.actions.write(
        output = config_file,
        content = config_content,
    )
    return config_file

# The config the generator writes sets these, and each is only reached through
# the attr named beside it. A server that does not read one is only a problem for
# a target that asked for it, so the check is per-attr rather than per-field.
_CONFIG_FIELD_ATTRS = {
    "server.open": "open",
    "root": None,
}

def _check_ignored_fields(ctx, server_info):
    """Fails when a set attr reaches a config field this server does not read."""
    for field in server_info.ignored_config_fields:
        attr_name = _CONFIG_FIELD_ATTRS.get(field)
        if not attr_name:
            continue
        if getattr(ctx.attr, attr_name, None):
            fail(
                "ts_dev_server: {} sets {} = {}, which reaches the generated config as `{}`.\n".format(
                    ctx.label,
                    attr_name,
                    getattr(ctx.attr, attr_name),
                    field,
                ) +
                "The server this target selected ({}) does not read that field, so the\n".format(
                    ctx.attr.server.label,
                ) +
                "setting would be silently dropped rather than applied.\n" +
                "Either drop the attr, or select a server that reads it:\n" +
                "    server = \"@rules_typescript//vite:dev_server\"",
            )

def _resolve_server(ctx):
    """Reads DevServerInfo off the server attr and checks it is self-consistent."""
    server_info = ctx.attr.server[DevServerInfo]
    if server_info.config_dialect != "vite":
        fail(
            "ts_dev_server: server '{}' declares config_dialect '{}'.\n".format(
                ctx.attr.server.label,
                server_info.config_dialect,
            ) +
            "This rule only generates a Vite-dialect config; a server reading another\n" +
            "format needs a generator for it before it can be selected here.",
        )
    if bool(server_info.server_binary) == bool(server_info.server_in_tree):
        fail(
            "ts_dev_server: server '{}' must set exactly one of DevServerInfo.server_binary ".format(
                ctx.attr.server.label,
            ) +
            "(a native or built executable) and DevServerInfo.server_in_tree (a path inside " +
            "the node_modules tree); it set " +
            ("both" if server_info.server_binary else "neither") + ".",
        )
    if ctx.attr.react_refresh and server_info.native_react_refresh:
        fail(
            "ts_dev_server: {} sets react_refresh = True, but the server it selected ({})\n".format(
                ctx.label,
                ctx.attr.server.label,
            ) +
            "applies React Fast Refresh itself. Adding @vitejs/plugin-react on top would\n" +
            "instrument every component a second time.\n" +
            "Drop react_refresh; Fast Refresh is already on.",
        )
    _check_ignored_fields(ctx, server_info)
    return server_info

def _ts_dev_server_impl(ctx):
    entry = ctx.attr.entry_point[TsInfo]
    server_info = _resolve_server(ctx)

    js_runtime = get_js_runtime(ctx)
    if not js_runtime:
        fail(
            "ts_dev_server: no JS runtime toolchain resolved for '{}'.\n".format(ctx.label) +
            "The dev server runs the toolchain's Node binary out of runfiles; it does " +
            "not fall back to whatever `node` is on your PATH.\n" +
            "Did you mean to register the toolchains in MODULE.bazel?\n" +
            "    register_toolchains(\"@rules_typescript//ts/toolchain:all\")",
        )
    runtime_binary = js_runtime.runtime_binary
    runtime_args = js_runtime.args_prefix

    node_modules = ctx.attr.node_modules
    node_modules_files = depset()
    if node_modules:
        node_modules_files = node_modules[DefaultInfo].files

    plugin_files = ctx.files.plugin
    plugin_rl = ""
    if plugin_files:
        plugin_rl = rlocation_path(ctx, plugin_files[0])

    # A copy in bin, not the source file: Node resolves the runfiles symlink
    # before that file's own imports, which would then leave the Bazel tree.
    staged_config = stage_vite_config(
        ctx,
        ctx.file.vite_config,
        ctx.files.vite_config_srcs,
        "{}_dev/config".format(ctx.label.name),
    )
    user_config = staged_config.entry
    user_config_rl = rlocation_path(ctx, user_config) if user_config else ""

    react_refresh = ctx.attr.react_refresh
    server_binary_rl = ""
    if server_info.server_binary:
        server_binary_rl = rlocation_path(ctx, server_info.server_binary)
    live_data = {file: True for file in entry.transitive_data.to_list()}
    live_runtime = live_data | {file: True for file in depset(transitive = [entry.transitive_js, entry.transitive_runtime_sources]).to_list()}
    declared_files = {}
    original_sources = []
    owners = entry.owners.to_list()
    links = runtime_links(owners)
    for owner in owners:
        for original, logical, published in getattr(owner, "asset_files", ()):
            if published not in live_data:
                continue
            _add_declared_file(declared_files, logical, original, canonical_runtime_file(published, links), "asset")
    for owner in owners:
        for source, runtime in getattr(owner, "runtime_files", ()):
            if runtime not in live_runtime:
                continue
            original_sources.append(source)

            _add_declared_file(declared_files, _logical_path(source), source, source, "source")
        for source, runtime in getattr(owner, "runtime_scopes", ()):
            if runtime in live_data:
                original_sources.append(source)
                _add_declared_file(declared_files, _logical_path(source), source, source, "source")

    # Prior owner records expose member links through npm_files without binding metadata.
    npm_files = entry.npm_files.to_list()
    npm_contexts = check_dev_server_npm_contexts(ctx, owners, live_runtime, node_modules[NodeModulesInfo] if node_modules else None, npm_files)

    config_file = _generate_dev_config(
        ctx,
        bool(node_modules),
        plugin_rl,
        react_refresh,
        rlocation_path(ctx, runtime_binary),
        _server_config_input_js(server_info, server_binary_rl),
        declared_files,
        npm_contexts.contexts,
        npm_contexts.views,
        user_config_rl,
    )

    # A server shipped as an npm package is a path under the node_modules
    # directory, joined by the launcher; a native server is a File.
    dev_server = {
        "config_file": rlocation_path(ctx, config_file),
        "package_dir": ctx.label.package,
        "argv": server_info.argv,
        "runs_in_js_runtime": server_info.runs_in_js_runtime,
        "port": ctx.attr.port,
        # The same directory the generated config points cacheDir at: one place
        # under bazel-bin for whatever a dev server insists on writing.
        "scratch_dir": _logical_path(config_file).rsplit("/", 1)[0],
    }
    if server_info.server_in_tree:
        dev_server["server_in_tree"] = server_info.server_in_tree
    else:
        dev_server["server_binary"] = server_binary_rl
    if node_modules:
        config_key = rlocation_path(ctx, config_file)
        view_name = "/".join(["part/" + part for part in config_key.split("/")])

        # Canonical repository names exclude @; this private root owns dev npm views.
        dev_server["node_modules"] = "@rules_typescript_dev/" + view_name + "/node_modules"
    if plugin_files:
        dev_server["plugin"] = plugin_rl
    if user_config:
        dev_server["user_config"] = user_config_rl

    launcher = declare_launcher(ctx, {
        "label": str(ctx.label),
        "mode": "devserver",
        "workspace": ctx.workspace_name,
        "runtime": rlocation_path(ctx, runtime_binary),
        "runtime_args": runtime_args,
        "dev_server": dev_server,
    })

    explicit_runfiles = [config_file, runtime_binary] + launcher.files
    explicit_runfiles.extend(plugin_files)
    explicit_runfiles.extend(staged_config.files)

    transitive_runfiles = depset(
        [runtime_binary],
        transitive = [
            entry.transitive_js,
            entry.transitive_runtime_sources,
            depset(original_sources, transitive = [entry.sources], order = "postorder"),
            entry.transitive_js_maps,
            entry.transitive_data,
            entry.npm_files,
            server_info.runtime_deps,
            node_modules_files,
            npm_contexts.files,
        ],
        order = "postorder",
    )
    root_symlinks = dict(launcher.root_symlinks)
    view_entries = [("node_modules/" + name, binding.target) for name, binding in npm_contexts.links.items()]
    for member, stores in npm_contexts.members.items():
        view_entries.extend([(member + "/" + name, store) for name, store in stores.items()])
    for relative, target in view_entries:
        path = "@rules_typescript_dev/" + view_name + "/" + relative
        if target.is_symlink:
            alias = ctx.actions.declare_symlink("_rules_typescript_dev/" + view_name + "/" + relative)

            # Bazel republishes unresolved link text here; private and canonical roots share no path components.
            ctx.actions.symlink(output = alias, target_path = "../" * (len(path.split("/")) - 1) + rlocation_path(ctx, target))

            # Bazel reads unresolved-link metadata only from runfiles.files.
            explicit_runfiles.append(alias)
            target = alias
        root_symlinks[path] = target

    runfiles = ctx.runfiles(
        files = explicit_runfiles,
        root_symlinks = root_symlinks,
        transitive_files = transitive_runfiles,
    )

    return [
        DefaultInfo(
            executable = launcher.executable,
            files = depset([config_file]),
            runfiles = runfiles,
        ),
    ]

ts_dev_server = rule(
    implementation = _ts_dev_server_impl,
    executable = True,
    fragments = ["platform"],
    toolchains = LAUNCHER_TOOLCHAINS + [
        config_common.toolchain_type(JS_RUNTIME_TOOLCHAIN_TYPE, mandatory = False),
    ],
    attrs = {
        "entry_point": attr.label(
            doc = "The ts_compile target that is the application entry point.",
            providers = [TsInfo],
            mandatory = True,
        ),
        "node_modules": attr.label(
            doc = "The importer's `node_modules` target, linking " +
                  "every package the application imports. The generated " +
                  "config points module resolution at its directory.",
            providers = [NodeModulesInfo],
        ),
        "plugin": attr.label(
            doc = "Optional compiled vite-plugin-bazel JavaScript file. " +
                  "When set (e.g. '//vite:vite_plugin_bazel'), the generated vite.config.mjs " +
                  "imports the plugin, which resolves generated code out of bazel-bin, " +
                  "invalidates precisely on a rebuild, and restarts the server when the " +
                  "config it was generated from changes. Without this attr Vite serves " +
                  "first-party source and nothing else: bazel-bin, and therefore ts_codegen " +
                  "output, is invisible to it. This attr accepts a bundled .mjs file target.",
            allow_single_file = [".mjs", ".js"],
        ),
        "port": attr.int(
            doc = "Port for the dev server. Default: 5173.",
            default = 5173,
        ),
        "host": attr.string(
            doc = "Host to bind the dev server to. Default: 'localhost'. " +
                  "Set to '0.0.0.0' to bind on all interfaces.",
            default = "localhost",
        ),
        "open": attr.bool(
            doc = "Whether to open the browser automatically when the dev server starts.",
            default = False,
        ),
        "server": attr.label(
            doc = "Which dev server implementation serves this target, as a " +
                  "target providing DevServerInfo. Defaults to source-built oj; any other " +
                  "rule returning the provider reads the same generated config. " +
                  "A server that " +
                  "does not read a config field this target set is an analysis-" +
                  "time error naming both, so switching implementations cannot " +
                  "silently drop a setting.",
            default = "@rules_typescript//oj:dev_server",
            providers = [DevServerInfo],
        ),
        "react_refresh": attr.bool(
            doc = "Enable React Fast Refresh via @vitejs/plugin-react. " +
                  "When True, the generated vite.config.mjs imports and uses " +
                  "@vitejs/plugin-react so that React component state is preserved " +
                  "across HMR updates instead of being lost on every file change. " +
                  "Requires @vitejs/plugin-react to be included in the node_modules attr. " +
                  "Example: add '@npm//:vitejs_plugin-react' to your node_modules() deps.",
            default = False,
        ),
        "vite_config": attr.label(
            doc = "Optional user-supplied Vite plugin configuration file (.mjs or .js). " +
                  "When set, the generated vite.config.mjs imports this file and prepends " +
                  "its plugins to the Bazel system plugins (react, bazel-plugin, npm " +
                  "resolution), which is how a framework plugin (TanStack Start, Remix) " +
                  "gets to transform a module first. The file must export a default object " +
                  "with a `plugins` array: " +
                  "  export default { plugins: [myFrameworkPlugin()] }; " +
                  "\n\n" +
                  "What such a config may import is a hard boundary, because the rule " +
                  "loads a COPY of it in bazel-bin (a source-tree config would resolve " +
                  "its own imports through a source-tree node_modules, which this ruleset " +
                  "does not have): a bare npm specifier resolves through the tree the " +
                  "node_modules attr built, as long as that target is in the same Bazel " +
                  "package as this one -- that is the directory Node finds walking up from " +
                  "the copy. A RELATIVE import does not resolve: only this one file is " +
                  "copied, so a sibling module is not there to be found, and the load " +
                  "fails with a `[rules_typescript] Failed to load vite_config` error " +
                  "naming it. Keep the config self-contained, or reach the tree explicitly " +
                  "through the NODE_MODULES_PATH environment variable the launcher sets. " +
                  "The copy's path reaches the generated config as VITE_USER_CONFIG_PATH.",
            allow_single_file = VITE_CONFIG_EXTENSIONS,
        ),
        "vite_config_srcs": attr.label_list(
            doc = VITE_CONFIG_SRCS_DOC,
            allow_files = True,
        ),
    },
    doc = """Starts source-built oj for a TypeScript application.

The server transforms first-party source without a Bazel rebuild. Typechecking
remains in the editor and `bazel build`. The `node_modules` attribute supplies
the packages the application imports.

Set `plugin = "@rules_typescript//vite:vite_plugin_bazel"` to resolve generated
files from bazel-bin and reload them after a rebuild. `ibazel run` can rebuild
those files while the server keeps running.

Select `server = "@rules_typescript//vite:dev_server"` to use Vite and include
Vite in `node_modules`. For Vite React Fast Refresh, also set
`react_refresh = True` and include `@vitejs/plugin-react`. oj supplies React
Fast Refresh itself; leave `react_refresh` unset with oj.

Example:

    ts_dev_server(
        name = "dev",
        entry_point = ":app",
        node_modules = ":node_modules",
        plugin = "@rules_typescript//vite:vite_plugin_bazel",
    )
""",
)
