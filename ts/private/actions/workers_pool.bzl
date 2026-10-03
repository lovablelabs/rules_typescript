"""The Workers pool: vitest's tests inside workerd.

The pool's half of a ts_test's environment, in one file: its attributes, the
WranglerTestConfig action and the symlink it adds. ts_test reaches it through
the struct workers_pool_environment returns, and no other file names wrangler.
"""

load("//tools/launcher:launcher.bzl", "runfiles_link_path")
load("//ts/private:runtime.bzl", "get_js_tool")

WORKERS_POOL_ATTRS = {
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
        if not chain.dirs:
            fail(("ts_test {}: wrangler_config needs `node_modules`, the " +
                  "importer whose links hold the pool; wrangler is the " +
                  "pool's own edge.").format(ctx.label))
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
        args.add_all(chain.dirs, before_each = "--node-modules")
        ctx.actions.run(
            inputs = depset(
                [src, ctx.file._wrangler_patch],
                transitive = [chain.npm_files],
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
