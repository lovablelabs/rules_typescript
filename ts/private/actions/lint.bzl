"""Lint as one validation action per compile, configured once at the module.

The root module's ts.lint(binary, config, fail_on_warnings) writes the
@lint_config repository; its one target is what the //ts:lint label flag
names, and compile_program runs TsLint over the program's sources when it
names a binary. docs/guides/lint.md is the reference.
"""

load("//ts/private:node_modules.bzl", "importer_chain")
load("//ts/private:providers.bzl", "NodeModulesInfo")
load("//ts/private:toolchain.bzl", "get_tools_toolchain")
load(":tsgo.bzl", "program_args", "program_inputs", "source_path")

LintConfigInfo = provider(
    doc = "What //ts:lint resolves to: the linter every compile runs.",
    fields = {
        "binary": "FilesToRunProvider, or None when nothing lints.",
        "config": "File passed as --config, or None.",
        "tool_env": "dict of environment name to FilesToRunProvider: auxiliary executables.",
        "data": "depset of File: declared config imports, plugins and ignore files.",
        "importers": "list of string: importer directories declared in data, root last.",
        "args": "list of string: linter arguments; {tsconfig} names the action config.",
        "fail_on_warnings": "bool: --max-warnings=0 is passed.",
    },
)

def _lint_config_impl(ctx):
    binary = None
    if ctx.attr.binary:
        binary = ctx.attr.binary[DefaultInfo].files_to_run
    tool_env = {}
    for target, name in ctx.attr.tool_env.items():
        if not name or name[0] not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_" or any([c not in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_0123456789" for c in name.elems()]):
            fail("tool_env for {} needs an environment variable name, got {}".format(target.label, repr(name)))
        if name in tool_env:
            fail("tool_env assigns {} to more than one executable".format(name))
        tool = target[DefaultInfo].files_to_run
        if not tool or not tool.executable:
            fail("tool_env target {} must be executable".format(target.label))
        tool_env[name] = tool
    importers = {}
    links = []
    stores = []
    for dep in ctx.attr.data:
        if NodeModulesInfo in dep:
            for importer in importer_chain(dep[NodeModulesInfo]):
                importers[importer.dir] = True
                for entry in importer.links.values():
                    links.append(entry.link)
                    stores.append(entry.store.transitive)
    return [LintConfigInfo(
        binary = binary,
        tool_env = tool_env,
        config = ctx.file.config,
        data = depset(ctx.files.data + links, transitive = stores),
        importers = importers.keys(),
        args = ctx.attr.args,
        fail_on_warnings = ctx.attr.fail_on_warnings,
    )]

lint_config = rule(
    implementation = _lint_config_impl,
    attrs = {
        "binary": attr.label(
            doc = "The linter's executable, `@npm//:oxlint_bin` or " +
                  "`@npm//:eslint_bin`; unset, no target lints.",
            executable = True,
            cfg = "exec",
        ),
        "config": attr.label(
            doc = "The linter's own config file, passed as --config; " +
                  "unset, the flag is not passed.",
            allow_single_file = True,
        ),
        "tool_env": attr.label_keyed_string_dict(
            cfg = "exec",
            doc = "Executable labels mapped to environment names receiving their absolute paths.",
        ),
        "data": attr.label_list(allow_files = True),
        "args": attr.string_list(),
        "fail_on_warnings": attr.bool(
            doc = "A warning fails the build: --max-warnings=0, which " +
                  "oxlint and eslint spell alike.",
        ),
    },
    doc = "The linter //ts:lint names; ts.lint() writes the one at " +
          "@lint_config//:lint.",
)

def _lint_config_repo_impl(repository_ctx):
    attrs = repository_ctx.attr
    lines = ['    name = "lint",']
    if attrs.binary:
        lines.append('    binary = "{}",'.format(attrs.binary))
    if attrs.config:
        lines.append('    config = "{}",'.format(attrs.config))
    lines.append("    tool_env = {},".format(repr(attrs.tool_env)))
    lines.append("    data = {},".format(repr(attrs.data)))
    lines.append("    args = {},".format(repr(attrs.args)))
    lines.append("    fail_on_warnings = {},".format(attrs.fail_on_warnings))
    lines.append('    visibility = ["//visibility:public"],')
    repository_ctx.file(
        "BUILD.bazel",
        'load("{}", "lint_config")\n\nlint_config(\n{}\n)\n'.format(
            str(Label("//ts/private/actions:lint.bzl")),
            "\n".join(lines),
        ),
    )

lint_config_repo = repository_rule(
    implementation = _lint_config_repo_impl,
    attrs = {
        "binary": attr.string(
            doc = "The linter's label, canonical; \"\" for none.",
        ),
        "config": attr.string(
            doc = "The config file's label, canonical; \"\" for none.",
        ),
        "tool_env": attr.string_dict(),
        "data": attr.string_list(),
        "args": attr.string_list(),
        "fail_on_warnings": attr.bool(),
    },
    doc = "The repository holding the lint_config a root module's ts.lint() " +
          "configured.",
)

def lint_action(ctx, lint, srcs, tsconfig, inputs, chain, dep_dts, npm_files, importers, overlays, manifests, generated_srcs = [], inherited_importers = []):
    """Registers TsLint in the same program layout as tsgo's validation."""
    stamp = ctx.actions.declare_file("{}.tslint".format(ctx.label.name))
    config_files = [lint.config] if lint.config else []
    own_importers = importers or lint.importers
    inherited = {
        directory: True
        for directory in inherited_importers + lint.importers
        if directory not in own_importers
    }
    args = program_args(
        ctx,
        "{}/{}.lint".format(stamp.dirname, ctx.label.name),
        inputs,
        chain,
        dep_dts,
        own_importers,
        overlays,
        manifests,
        generated_srcs,
        inherited.keys(),
    )
    args.add_all(depset(config_files, transitive = [lint.data]), map_each = source_path, format_each = "-copy=%s", expand_directories = False)
    if tsconfig:
        args.add(tsconfig, format = "-discover-tsconfig=%s")
    for name, tool in lint.tool_env.items():
        args.add(tool.executable, format = "-tool-env=" + name + "=%s")
    args.add(stamp, format = "-stamp=%s")
    args.add("--")
    args.add(lint.binary.executable)
    if lint.config:
        args.add("--config", lint.config)
    if lint.fail_on_warnings:
        args.add("--max-warnings=0")
    for arg in lint.args:
        if "{tsconfig}" in arg and not tsconfig:
            fail("lint args require {tsconfig}, but {} has no runtime sources".format(ctx.label))
        args.add(arg.replace("{tsconfig}", tsconfig.path if tsconfig else ""))
    args.add_all(srcs)
    ctx.actions.run(
        inputs = depset(
            config_files,
            transitive = [program_inputs(tsconfig, inputs, chain, dep_dts, npm_files), lint.data],
        ),
        tools = [lint.binary] + lint.tool_env.values(),
        outputs = [stamp],
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = ["tsgo", args],
        env = {"PATH": "/bin:/usr/bin"},
        mnemonic = "TsLint",
        progress_message = "TsLint %{label}",
    )
    return stamp
