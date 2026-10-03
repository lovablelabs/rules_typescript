"""Real loader consumers of a borrowed emitted module supplied only through data."""

load("@rules_shell//shell:sh_test.bzl", "sh_test")
load("//ts:defs.bzl", "BundlerInfo", "ts_binary", "ts_test")

def _esbuild_adapter_impl(ctx):
    return [BundlerInfo(
        bundler_binary = ctx.attr.binary[DefaultInfo].files_to_run,
        runtime_deps = depset(),
        config_file = None,
        use_generated_config = False,
    )]

esbuild_adapter = rule(
    implementation = _esbuild_adapter_impl,
    attrs = {"binary": attr.label(executable = True, cfg = "exec", mandatory = True)},
)

def _binary_runfiles_impl(ctx):
    return [DefaultInfo(
        files = depset([ctx.executable.binary]),
        runfiles = ctx.attr.binary[DefaultInfo].default_runfiles,
    )]

# A bundled binary's default files are its bundle outputs, not its executable.
_binary_runfiles = rule(
    implementation = _binary_runfiles_impl,
    attrs = {"binary": attr.label(executable = True, cfg = "target", mandatory = True)},
)

def data_closure_tests():
    data = [":source_data", "//tests/node_test/data_closure/adapter:helper"]
    for consumer in ["unbundled", "bundled", "node", "vitest"]:
        name = consumer + "_data_closure_test"
        if consumer in ["unbundled", "bundled"]:
            binary = consumer + "_data"
            ts_binary(
                name = binary,
                entry_point = ":entry",
                entry_file = "binary.mjs",
                data = data,
                bundler = ":bundler" if consumer == "bundled" else None,
                bundle_name = "binary",
                sourcemap = False,
            )
            executable = binary + "_executable"
            _binary_runfiles(name = executable, binary = ":" + binary)
            sh_test(
                name = name,
                srcs = ["run_binary.sh"],
                args = ["$(rootpath :{})".format(executable)],
                data = [":" + executable],
            )
        else:
            source = consumer + ".test.mjs"
            ts_test(
                name = name,
                srcs = [source, "value.cjs"],
                test_srcs = [source],
                data = data + ["helper.cjs"],
                emit = True,
                node_modules = ":node_modules",
                runner = "//ts/runners:node_test" if consumer == "node" else "//ts/runners:vitest",
                deps = [
                    ":check",
                    "@npm//:types_node",
                ] + (["@npm//:vitest"] if consumer == "vitest" else []),
            )
