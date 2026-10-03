"""The gazelle_roundtrip fixture generated once, as a build action every group reads."""

load(
    "//npm/private:npm_translate_lock.bzl",
    "DEFAULT_NPM_REGISTRY",
    "npm_tarball_url",
    "parse_pnpm_lock",
    "verify_integrity",
)

def _gazelle_roundtrip_tarballs_impl(rctx):
    packages = parse_pnpm_lock(rctx.read(rctx.attr.pnpm_lock))["packages"]
    unverifiable = verify_integrity(packages)
    if unverifiable:
        fail("{}: no registry integrity for {}".format(rctx.attr.pnpm_lock, unverifiable[0].package_id))
    pending, paths = [], []

    # Every platform's: pnpm picks the exec platform's, wherever that is.
    for package in packages.values():
        url = npm_tarball_url(package["name"], package["version"], package["resolution"])
        path = url.removeprefix(DEFAULT_NPM_REGISTRY + "/")
        if path == url:
            fail("{}: {} is not on the default registry".format(rctx.attr.pnpm_lock, url))
        pending.append(rctx.download(url, "registry/" + path, integrity = package["resolution"]["integrity"], block = False))
        paths.append(path)
    for download in pending:
        download.wait()
    rctx.file("registry/tarballs.txt", "\n".join(sorted(paths)) + "\n")
    rctx.file("BUILD.bazel", """filegroup(
    name = "registry",
    srcs = ["registry/tarballs.txt"] + glob(["registry/**/*.tgz"]),
    visibility = ["//visibility:public"],
)
""")

gazelle_roundtrip_tarballs = repository_rule(
    implementation = _gazelle_roundtrip_tarballs_impl,
    attrs = {"pnpm_lock": attr.label(mandatory = True, allow_single_file = True)},
    doc = "Every registry tarball a pnpm-lock.yaml names, at the registry's own paths.",
)

def _tsgo_source_impl(settings, _attr):
    return {
        "//command_line_option:extra_toolchains": settings["//command_line_option:extra_toolchains"] + [str(Label("//ts/toolchain/tsgo_source"))],
    }

# The nested workspace registers the source-built tsgo first; Gazelle lists with the same compiler here.
_tsgo_source = transition(
    implementation = _tsgo_source_impl,
    inputs = ["//command_line_option:extra_toolchains"],
    outputs = ["//command_line_option:extra_toolchains"],
)

def _tsgo_source_tool_impl(ctx):
    actual = ctx.attr.actual[0][DefaultInfo]
    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = executable, target_file = actual.files_to_run.executable, is_executable = True)
    return [DefaultInfo(
        executable = executable,
        runfiles = actual.default_runfiles.merge(ctx.runfiles(files = [executable])),
    )]

tsgo_source_tool = rule(
    implementation = _tsgo_source_tool_impl,
    doc = "An executable built with the source-built tsgo registered first; an exec dep on it composes the two.",
    executable = True,
    attrs = {
        "actual": attr.label(mandatory = True, executable = True, cfg = _tsgo_source),
        "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
    },
)

def _gazelle_roundtrip_workspace_impl(ctx):
    workspace = ctx.actions.declare_directory(ctx.label.name)
    install = ctx.actions.declare_file(ctx.label.name + ".install.tar")
    log = ctx.actions.declare_file(ctx.label.name + ".gazelle.log")
    gazelle = ctx.attr.gazelle[DefaultInfo].files_to_run
    registry = [file for file in ctx.files.registry if file.basename == "tarballs.txt"]
    files = ctx.actions.args()
    files.set_param_file_format("multiline")
    files.use_param_file("-files=%s", use_always = True)
    files.add_all(ctx.files.srcs)
    args = ctx.actions.args()
    args.add("generate")
    args.add("-fixture", ctx.attr.fixture)
    args.add("-workers_lock", ctx.file.workers_lock)
    args.add("-pnpm", ctx.executable.pnpm)
    args.add("-registry", registry[0])
    args.add("-gazelle", gazelle.executable)
    args.add("-out", workspace.path)
    args.add("-install", install)
    args.add("-log", log)
    ctx.actions.run(
        executable = ctx.executable.generator,
        arguments = [args, files],
        inputs = depset(ctx.files.srcs + ctx.files.registry + [ctx.file.workers_lock]),
        tools = [gazelle, ctx.executable.pnpm],
        outputs = [workspace, install, log],
        mnemonic = "GazelleRoundtripWorkspace",
        # The Gazelle runner is a bash script found through PATH; a bare action env has none.
        use_default_shell_env = True,
        progress_message = "Generating the gazelle_roundtrip workspace %{label}",
    )
    return [DefaultInfo(files = depset([workspace, install, log]))]

gazelle_roundtrip_workspace = rule(
    implementation = _gazelle_roundtrip_workspace_impl,
    doc = "The fixture staged, installed and through one Gazelle pass; the install is the `.install.tar` beside it.",
    attrs = {
        "fixture": attr.string(mandatory = True, doc = "The fixture's directory, the prefix of every src."),
        "gazelle": attr.label(mandatory = True, executable = True, cfg = "exec", doc = "A tsgo_source_tool over the gazelle() runner."),
        "generator": attr.label(mandatory = True, executable = True, cfg = "exec"),
        "pnpm": attr.label(mandatory = True, executable = True, allow_single_file = True, cfg = "exec"),
        "registry": attr.label(mandatory = True),
        "srcs": attr.label_list(mandatory = True, allow_files = True),
        "workers_lock": attr.label(mandatory = True, allow_single_file = True),
    },
)
