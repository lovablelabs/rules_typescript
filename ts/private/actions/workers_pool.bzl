"""The Workers pool: vitest's tests inside workerd.

The pool's half of a ts_test's environment, in one file: its attributes, the
WranglerTestConfig action and the symlink it adds. ts_test reaches it through
the struct workers_pool_environment returns, and no other file names wrangler.
"""

load("//tools/launcher:launcher.bzl", "runfiles_link_path")
load("//ts/private:providers.bzl", "NodeModulesInfo", "NpmLinkInfo")
load("//ts/private:runtime.bzl", "get_js_tool")

_POOL_PACKAGE = "@cloudflare/vitest-pool-workers"

WORKERS_POOL_ATTRS = {
    "workers_pool": attr.label(
        doc = "The node_modules importer or node_modules_member link declaring the pool imported by the " +
              "config or one of its helpers. When set, its pool supplies " +
              "Wrangler and is staged for runtime. Otherwise Wrangler uses " +
              "the first pool on the test's importer chain. Gazelle selects " +
              "the explicit owner from the resolved config import.",
        providers = [[NodeModulesInfo], [NpmLinkInfo]],
    ),
    "wrangler_config": attr.label(
        doc = "The wrangler config a Workers-pool `config` names through " +
              "`wrangler.configPath`. A copy projecting matching `main` and " +
              "`env.<name>.main` entries to declared runtime artifacts is " +
              "staged at this file's own runfiles path; the file is not " +
              "also listed in `data`.",
        allow_single_file = [".jsonc", ".json", ".toml"],
    ),
    "_wrangler_patch": attr.label(
        default = Label("//ts/private:wrangler_test_config.mjs"),
        allow_single_file = True,
    ),
}

def _pool_chain(ctx, chain):
    if not ctx.attr.workers_pool:
        return chain
    owner = ctx.attr.workers_pool
    pool = None
    if NpmLinkInfo in owner:
        if owner[NpmLinkInfo].link.path.endswith("/node_modules/" + _POOL_PACKAGE):
            pool = owner[NpmLinkInfo]
    else:
        pool = owner[NodeModulesInfo].links.get(_POOL_PACKAGE)
    if pool == None:
        fail(("ts_test {}: workers_pool {} does not link {}. Did you mean " +
              "the importer or member link declaring the config's pool?").format(ctx.label, owner.label, _POOL_PACKAGE))
    return struct(
        dirs = [pool.link.path[:-len("/" + _POOL_PACKAGE)]],
        npm_files = owner[DefaultInfo].files,
    )

def workers_pool_environment(ctx, chain, runtime_data_sets, runtime_files, asset_files, runtime_sources, runtime_js):
    symlinks = {}
    replacements = {}

    if ctx.file.wrangler_config:
        src = ctx.file.wrangler_config
        bindings = {src: True}
        bindings.update({runtime: True for source, runtime in runtime_files if source == src})
        bindings.update({published: True for original, _coordinate, published in asset_files if original == src})
        entries = {}
        for source, runtime in runtime_files:
            previous = entries.get(source.short_path)
            if previous != None and previous != runtime:
                fail("ts_test {}: source '{}' has conflicting declared runtime owners '{}' and '{}'.".format(ctx.label, source.short_path, previous.path, runtime.path))
            entries[source.short_path] = runtime

        runtime_data_sets = [depset([
            f
            for f in depset(transitive = runtime_data_sets).to_list()
            if f not in bindings
        ])]

        js_tool = get_js_tool(ctx)
        if not js_tool:
            fail(("ts_test {}: wrangler_config needs the JS tool toolchain " +
                  "to patch the config.").format(ctx.label))
        pool = _pool_chain(ctx, chain)
        if not pool.dirs:
            fail(("ts_test {}: wrangler_config needs a Workers pool importer. " +
                  "Did you mean to set workers_pool to the importer or member link " +
                  "declaring the config's pool, or node_modules for the test's chain?").format(ctx.label))
        patched = ctx.actions.declare_file(
            "_{}_wrangler.{}".format(ctx.label.name, src.extension),
        )
        args = ctx.actions.args()
        args.add(ctx.file._wrangler_patch)
        args.add("--config", src)
        args.add("--out", patched)
        args.add("--config-path", src.short_path)
        args.add_all([json.encode([source, runtime.short_path]) for source, runtime in entries.items()], before_each = "--runtime-file")

        # Older owners publish runtime Files without source/runtime pairs.
        mapped = {runtime: True for _source, runtime in runtime_files}
        args.add_all([file.short_path for file in runtime_sources.to_list() if file not in mapped and not file.is_directory], before_each = "--runtime-source")
        args.add_all([file.short_path for file in runtime_js.to_list() if file not in mapped and not file.is_directory], before_each = "--runtime-js")
        args.add_all(pool.dirs, before_each = "--node-modules")
        ctx.actions.run(
            inputs = depset(
                [src, ctx.file._wrangler_patch],
                transitive = [pool.npm_files],
                order = "postorder",
            ),
            outputs = [patched],
            executable = js_tool.runtime_binary,
            arguments = js_tool.args_prefix + [args],
            mnemonic = "WranglerTestConfig",
            progress_message = "WranglerTestConfig %{label}",
        )
        symlinks[runfiles_link_path(src)] = patched
        replacements = {file: patched for file in bindings}

    return struct(
        symlinks = symlinks,
        runtime_data_sets = runtime_data_sets,
        replacements = replacements,
    )
