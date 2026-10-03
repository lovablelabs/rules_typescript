def _external_cjs_impl(rctx):
    rctx.file("helper.ts", "export const answer = (): number => 42;\n")
    rctx.file("external_cjs.test.ts", rctx.read(rctx.attr.test_source))
    rctx.file("vitest.config.mjs", "export default { root: process.env.RUNFILES_DIR };\n")
    rctx.file("package.json", '{"name":"external-commonjs","private":true}\n')
    rctx.file("tsconfig.json", '{"compilerOptions":{"module":"commonjs"}}\n')
    rctx.file("BUILD.bazel", """load("@rules_typescript//ts:defs.bzl", "ts_compile", "ts_config")

package(default_visibility = ["//visibility:public"])

exports_files(["external_cjs.test.ts", "vitest.config.mjs"])

ts_config(
    name = "tsconfig",
    src = "tsconfig.json",
    module = "commonjs",
)

ts_compile(
    name = "library",
    srcs = ["helper.ts"],
    emit = True,
    package_scopes = ["package.json"],
    tsconfig = ":tsconfig",
)
""")

external_cjs = repository_rule(
    implementation = _external_cjs_impl,
    attrs = {
        "test_source": attr.label(default = "//tests/vitest/commonjs:external_cjs_source.ts", allow_single_file = True),
    },
)
