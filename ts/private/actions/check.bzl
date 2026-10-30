"""The type checker TsgoCheck runs, configured once at the module.

The root module's ts.check(binary, args) writes the @check_config repository;
its one target is what the //ts:check label flag names. With no binary,
TsgoCheck runs the tsgo toolchain's compiler. A configured checker is called
with `args` and then tsgo's command line, and has to print and exit as tsgo
does: the step parses its --explainFiles listing. `oj tsc` is one.
"""

CheckConfigInfo = provider(
    doc = "What //ts:check resolves to: the type checker TsgoCheck runs.",
    fields = {
        "binary": "FilesToRunProvider, or None when the tsgo toolchain's compiler checks.",
        "args": "list of string: arguments before tsgo's command line.",
    },
)

def _check_config_impl(ctx):
    binary = None
    if ctx.attr.binary:
        binary = ctx.attr.binary[DefaultInfo].files_to_run
    return [CheckConfigInfo(
        binary = binary,
        args = ctx.attr.args,
    )]

check_config = rule(
    implementation = _check_config_impl,
    attrs = {
        "binary": attr.label(
            doc = "The checker's executable; unset, the tsgo toolchain's " +
                  "compiler checks.",
            executable = True,
            cfg = "exec",
        ),
        "args": attr.string_list(
            doc = "Arguments passed before tsgo's command line, " +
                  "`[\"tsc\"]` for `oj tsc`.",
        ),
    },
    doc = "The type checker //ts:check names; ts.check() writes the one at " +
          "@check_config//:check.",
)

def _check_config_repo_impl(repository_ctx):
    attrs = repository_ctx.attr
    lines = ['    name = "check",']
    if attrs.binary:
        lines.append('    binary = "{}",'.format(attrs.binary))
    lines.append("    args = {},".format(repr(attrs.args)))
    lines.append('    visibility = ["//visibility:public"],')
    repository_ctx.file(
        "BUILD.bazel",
        'load("{}", "check_config")\n\ncheck_config(\n{}\n)\n'.format(
            str(Label("//ts/private/actions:check.bzl")),
            "\n".join(lines),
        ),
    )

check_config_repo = repository_rule(
    implementation = _check_config_repo_impl,
    attrs = {
        "binary": attr.string(
            doc = "The checker's label, canonical; \"\" for tsgo.",
        ),
        "args": attr.string_list(),
    },
    doc = "The repository holding the check_config a root module's " +
          "ts.check() configured.",
)
