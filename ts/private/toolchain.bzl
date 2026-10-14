"""Toolchain definitions for rules_typescript.

The oxc and tsgo compilers, the Go tools the actions run (tsaction, the lcov
merger, the workspace copier) and the launcher every executable runs through,
with the repository rules that fetch a compiler binary and a tools release.
"""

load("//npm/private:fetch_auth.bzl", "fetch_auth")
load("//platforms:platforms.bzl", "constraints")

# Label(), not a string: these resolve in this file's repository mapping, so
# they keep working when a consumer gives rules_typescript another repo name.
OXC_TOOLCHAIN_TYPE = Label("//ts/toolchain:oxc_toolchain_type")
TSGO_TOOLCHAIN_TYPE = Label("//ts/toolchain:tsgo_toolchain_type")
TOOLS_TOOLCHAIN_TYPE = Label("//ts/toolchain:tools_toolchain_type")
LAUNCHER_TOOLCHAIN_TYPE = Label("//ts/toolchain:launcher_toolchain_type")

OxcToolchainInfo = provider(
    doc = "Information about the oxc-bazel toolchain.",
    fields = {
        "oxc_binary": "File: The oxc-bazel CLI binary.",
    },
)

TsgoToolchainInfo = provider(
    doc = "Information about the tsgo toolchain.",
    fields = {
        "tsgo_binary": "File: The tsgo CLI binary.",
        "files": "depset of Files: The compiler and its runtime data.",
        "manifest": "File or None: the package.json of the release the binary comes from.",
    },
)

ToolsInfo = provider(
    doc = "The Go tools the actions run on the exec platform.",
    fields = {
        "tsaction": "File: the runner behind the TsConfig, TsEmit, tsgo, " +
                    "TsLint, TsTestPaths, TsManifest and NpmStore actions.",
        "lcov_merger": "File: ts_test's coverage merger.",
        "copy_to_workspace": "File: the copier `bazel run` targets write " +
                             "the source tree with.",
    },
)

LauncherInfo = provider(
    doc = "The launcher a ts_test, ts_binary, ts_dev_server or npm_bin " +
          "executable is a symlink of; built for the target platform, it " +
          "runs where the program runs.",
    fields = {
        "launcher": "File: the launcher binary.",
    },
)

def _oxc_toolchain_impl(ctx):
    binary = ctx.file.oxc_binary
    toolchain_info = platform_common.ToolchainInfo(
        oxc_info = OxcToolchainInfo(
            oxc_binary = binary,
        ),
        # Standard fields consumed by @toolchain_utils//toolchain:resolved.bzl.
        executable = binary,
        variable = "OXC",
        default = DefaultInfo(
            files = depset([binary]),
            runfiles = ctx.runfiles([binary]),
        ),
    )
    return [toolchain_info]

oxc_toolchain = rule(
    implementation = _oxc_toolchain_impl,
    attrs = {
        "oxc_binary": attr.label(
            doc = "The oxc-bazel CLI binary.",
            mandatory = True,
            allow_single_file = True,
            executable = True,
            cfg = "exec",
        ),
    },
)

def _tsgo_toolchain_impl(ctx):
    binary = ctx.file.tsgo_binary
    files = depset([binary], transitive = [ctx.attr.data[DefaultInfo].files] if ctx.attr.data else [], order = "postorder")
    toolchain_info = platform_common.ToolchainInfo(
        tsgo_info = TsgoToolchainInfo(
            tsgo_binary = binary,
            files = files,
            manifest = ctx.file.manifest,
        ),
        # Standard fields consumed by @toolchain_utils//toolchain:resolved.bzl.
        executable = binary,
        variable = "TSGO",
        default = DefaultInfo(
            files = files,
            runfiles = ctx.runfiles(transitive_files = files),
        ),
    )
    return [toolchain_info]

tsgo_toolchain = rule(
    implementation = _tsgo_toolchain_impl,
    attrs = {
        "data": attr.label(allow_files = True, cfg = "exec"),
        "manifest": attr.label(
            doc = "The release's package.json, whose version Gazelle compares with the typescript-go it links in.",
            allow_single_file = True,
            cfg = "exec",
        ),
        "tsgo_binary": attr.label(
            doc = "The tsgo CLI binary.",
            mandatory = True,
            allow_single_file = True,
            executable = True,
            cfg = "exec",
        ),
    },
)

def _tools_toolchain_impl(ctx):
    files = [
        ctx.file.tsaction,
        ctx.file.lcov_merger,
        ctx.file.copy_to_workspace,
    ]
    runfiles = ctx.runfiles(files)
    for target in [
        ctx.attr.tsaction,
        ctx.attr.lcov_merger,
        ctx.attr.copy_to_workspace,
    ]:
        runfiles = runfiles.merge(target[DefaultInfo].default_runfiles)
    return [platform_common.ToolchainInfo(
        tools_info = ToolsInfo(
            tsaction = ctx.file.tsaction,
            lcov_merger = ctx.file.lcov_merger,
            copy_to_workspace = ctx.file.copy_to_workspace,
        ),
        executable = ctx.file.tsaction,
        variable = "TSACTION",
        default = DefaultInfo(files = depset(files), runfiles = runfiles),
    )]

def _tool_attr(doc):
    return attr.label(
        doc = doc,
        mandatory = True,
        allow_single_file = True,
        executable = True,
        cfg = "exec",
    )

tools_toolchain = rule(
    implementation = _tools_toolchain_impl,
    attrs = {
        "tsaction": _tool_attr("The tsaction binary."),
        "lcov_merger": _tool_attr("The lcov_merger binary."),
        "copy_to_workspace": _tool_attr("The copy_to_workspace binary."),
    },
    doc = "The Go action tools, built from source for the execution platform by default.",
)

def _launcher_toolchain_impl(ctx):
    launcher = ctx.file.launcher
    return [platform_common.ToolchainInfo(
        launcher_info = LauncherInfo(launcher = launcher),
        executable = launcher,
        variable = "TS_LAUNCHER",
        default = DefaultInfo(
            files = depset([launcher]),
            runfiles = ctx.runfiles([launcher]),
        ),
    )]

launcher_toolchain = rule(
    implementation = _launcher_toolchain_impl,
    attrs = {
        "launcher": attr.label(
            doc = "The launcher binary.",
            mandatory = True,
            allow_single_file = True,
            executable = True,
            cfg = "target",
        ),
    },
    doc = """The launcher every rules_typescript executable is a symlink of.

Built for the target platform, as `js_runtime_type`'s node is: it is staged
into the program's runfiles and runs wherever the program runs.
""",
)

def get_oxc_toolchain(ctx):
    """The resolved OxcToolchainInfo."""
    return ctx.toolchains[OXC_TOOLCHAIN_TYPE].oxc_info

def get_tsgo_toolchain(ctx):
    """The resolved TsgoToolchainInfo."""
    return ctx.toolchains[TSGO_TOOLCHAIN_TYPE].tsgo_info

def get_tools_toolchain(ctx):
    """The resolved ToolsInfo."""
    return ctx.toolchains[TOOLS_TOOLCHAIN_TYPE].tools_info

def get_launcher_toolchain(ctx):
    """The resolved LauncherInfo, or None when none resolved."""
    toolchain = ctx.toolchains[LAUNCHER_TOOLCHAIN_TYPE]
    if toolchain:
        return toolchain.launcher_info
    return None

# Keys of //platforms:platforms.bzl%PLATFORMS: the compiler packages and the
# tools release cover these four; Windows is unsupported (COMPATIBILITY.md).
TSGO_PLATFORMS = ["linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"]

def declare_oxc_toolchain(name):
    oxc_toolchain(
        name = "{}_impl".format(name),
        oxc_binary = Label("//oxc_cli:oxc-bazel"),
    )
    native.toolchain(
        name = name,
        toolchain = ":{}_impl".format(name),
        toolchain_type = OXC_TOOLCHAIN_TYPE,
    )

def declare_tsgo_source_toolchain(name):
    """Declares the tsgo toolchain over the compiler built from source.

    `@tsgo_source//:tsgo` is rules_go's build of the module
    ts/private/tsgo_source/go.mod pins, so one toolchain serves every exec
    platform and constrains none, as the oxc one does. //ts/toolchain:all
    does not reach it; a consumer registers it ahead of that pattern.

    Args:
        name: Name of the generated toolchain target.
    """
    tsgo_toolchain(
        name = "{}_impl".format(name),
        tsgo_binary = Label("@tsgo_source//:tsgo"),
    )
    native.toolchain(
        name = name,
        toolchain = ":{}_impl".format(name),
        toolchain_type = TSGO_TOOLCHAIN_TYPE,
    )

def declare_tsgo_toolchains(name, repo_prefix = None):
    """Declares one tsgo toolchain per platform tsgo publishes a binary for.

    tsgo is a compiler: it runs on the exec platform and places no constraint
    on the target platform.

    The per-platform `_impl` targets are tagged manual so that a `//...` build
    does not fetch every platform's tarball; toolchain resolution reaches the
    selected one directly.

    Args:
        name: Base name prefix for the generated targets.
        repo_prefix: Prefix of the per-platform external repositories.
                     Defaults to name.
    """
    if repo_prefix == None:
        repo_prefix = name
    for platform in TSGO_PLATFORMS:
        toolchain_name = "{}_{}".format(name, platform)
        tsgo_toolchain(
            name = "{}_impl".format(toolchain_name),
            tsgo_binary = "@{}_{}//:tsgo".format(repo_prefix, platform),
            data = "@{}_{}//:libs".format(repo_prefix, platform),
            manifest = "@{}_{}//:package.json".format(repo_prefix, platform),
            tags = ["manual"],
        )
        native.toolchain(
            name = toolchain_name,
            toolchain = ":{}_impl".format(toolchain_name),
            toolchain_type = TSGO_TOOLCHAIN_TYPE,
            exec_compatible_with = constraints(platform),
        )

def declare_tools_toolchains(name, repo_prefix = None):
    """Declares one tools toolchain per platform of the tools release.

    The tools run on the exec platform and place no constraint on the target
    platform. The `_impl` targets are manual, as the tsgo ones are.

    Args:
        name: Base name prefix for the generated targets.
        repo_prefix: Prefix of the per-platform external repositories.
                     Defaults to name.
    """
    if repo_prefix == None:
        repo_prefix = name
    for platform in TSGO_PLATFORMS:
        toolchain_name = "{}_{}".format(name, platform)
        repo = "@{}_{}".format(repo_prefix, platform)
        tools_toolchain(
            name = "{}_impl".format(toolchain_name),
            tsaction = repo + "//:tsaction",
            lcov_merger = repo + "//:lcov_merger",
            copy_to_workspace = repo + "//:copy_to_workspace",
            tags = ["manual"],
        )
        native.toolchain(
            name = toolchain_name,
            toolchain = ":{}_impl".format(toolchain_name),
            toolchain_type = TOOLS_TOOLCHAIN_TYPE,
            exec_compatible_with = constraints(platform),
        )

def declare_launcher_toolchains(name, repo_prefix = None):
    """Declares one launcher toolchain per platform of the tools release.

    The launcher runs where the program runs, so each is constrained on the
    target platform; a target platform outside the four resolves none.

    Args:
        name: Base name prefix for the generated targets.
        repo_prefix: Prefix of the per-platform external repositories.
                     Defaults to name.
    """
    if repo_prefix == None:
        repo_prefix = name
    for platform in TSGO_PLATFORMS:
        toolchain_name = "{}_{}".format(name, platform)
        launcher_toolchain(
            name = "{}_impl".format(toolchain_name),
            launcher = "@{}_{}//:ts_launcher".format(repo_prefix, platform),
            tags = ["manual"],
        )
        native.toolchain(
            name = toolchain_name,
            toolchain = ":{}_impl".format(toolchain_name),
            toolchain_type = LAUNCHER_TOOLCHAIN_TYPE,
            target_compatible_with = constraints(platform),
        )

TOOLS_BINARIES = ["tsaction", "ts_launcher", "lcov_merger", "copy_to_workspace"]

def _tools_toolchain_repo_impl(repository_ctx):
    url = repository_ctx.attr.url
    repository_ctx.download_and_extract(
        url = url,
        integrity = repository_ctx.attr.integrity,
        stripPrefix = repository_ctx.attr.strip_prefix,
    )
    for binary in TOOLS_BINARIES:
        if not repository_ctx.path(binary).exists:
            fail(("tools_toolchain_repo: the tools release at {} has no " +
                  "{}/{}; the assets of a tools-v<N> release carry {}.").format(
                url,
                repository_ctx.attr.strip_prefix,
                binary,
                ", ".join(TOOLS_BINARIES),
            ))
    repository_ctx.file("BUILD.bazel", (
        "exports_files({}, visibility = [\"//visibility:public\"])\n"
    ).format(repr(TOOLS_BINARIES)))

tools_toolchain_repo = repository_rule(
    implementation = _tools_toolchain_repo_impl,
    attrs = {
        "url": attr.string(
            doc = "The release asset: a tarball of the four binaries under " +
                  "`strip_prefix`.",
            mandatory = True,
        ),
        "integrity": attr.string(
            doc = "The asset's SRI integrity from ts/private/tools_lock.bzl.",
            mandatory = True,
        ),
        "strip_prefix": attr.string(
            doc = "The one directory the tarball holds.",
            mandatory = True,
        ),
    },
    doc = """Downloads the Go tools of one platform from a tools release.

One repository per platform, so that a build fetches only the tools it runs.
""",
)

def _auth_for_fetch(repository_ctx, url):
    npmrc = repository_ctx.attr.npmrc
    content = ""
    if npmrc:
        repository_ctx.watch(npmrc)
        content = repository_ctx.read(npmrc)
    return fetch_auth(
        content,
        url,
        repository_ctx.attr.package,
        repository_ctx.getenv,
        str(npmrc),
    )

def _tsgo_toolchain_repo_impl(repository_ctx):
    url = repository_ctx.attr.url
    integrity = repository_ctx.attr.integrity
    binary = repository_ctx.attr.binary

    download_kwargs = {
        "url": url,
        "stripPrefix": "package",
        "auth": _auth_for_fetch(repository_ctx, url),
    }
    if integrity:
        download_kwargs["integrity"] = integrity
    else:
        # buildifier: disable=print
        print("WARNING: tsgo from {} has no integrity; downloading unverified. ts.tsgo(pnpm_lock = ...) verifies every download.".format(url))
    repository_ctx.download_and_extract(**download_kwargs)

    if not repository_ctx.path("lib/" + binary).exists:
        fail("tsgo_toolchain_repo: the tarball at {} has no lib/{}. The platform packages of `typescript` ship lib/tsc and those of @typescript/native-preview lib/tsgo; ts.tsgo(package = ...) names which.".format(url, binary))

    # Platform-free label over the binary, so the toolchain macro does not have
    # to know the layout or which of the two compiler packages this is.
    repository_ctx.file("BUILD.bazel", """\
alias(
    name = "tsgo",
    actual = "lib/{binary}",
    visibility = ["//visibility:public"],
)

filegroup(
    name = "libs",
    srcs = glob(["lib/*.d.ts"]),
    visibility = ["//visibility:public"],
)

exports_files(["lib/{binary}", "package.json"])
""".format(binary = binary))

tsgo_toolchain_repo = repository_rule(
    implementation = _tsgo_toolchain_repo_impl,
    attrs = {
        "package": attr.string(
            doc = "The platform package the tarball holds, " +
                  "`@typescript/typescript-linux-x64` and the like; its " +
                  "scope picks the `PNPM_CONFIG__AUTH` entry.",
            mandatory = True,
        ),
        "url": attr.string(
            doc = "The platform package's tarball: `package/lib/<binary>` inside.",
            mandatory = True,
        ),
        "integrity": attr.string(
            doc = "The tarball's SRI integrity, as pnpm writes it. Empty downloads unverified.",
        ),
        "binary": attr.string(
            doc = "The compiler's file name under lib/: tsc or tsgo.",
            mandatory = True,
        ),
        "npmrc": attr.label(
            allow_single_file = True,
            doc = "The workspace .npmrc, read at fetch time for the credentials of the " +
                  "registry `url` points at, so no token is ever an attribute value.",
        ),
    },
    doc = """Downloads the TypeScript compiler binary for one platform.

One repository per platform, so that a build fetches only the compiler it runs.
""",
)

def _tsgo_source_repo_impl(repository_ctx):
    repository_ctx.file("BUILD.bazel", """\
alias(
    name = "tsgo",
    actual = "{binary}",
    visibility = ["//visibility:public"],
)
""".format(binary = repository_ctx.attr.binary))
    repository_ctx.file("defs.bzl", """\
TSGO_SOURCE_MODULE = "{module}"
TSGO_SOURCE_VERSION = "{version}"
""".format(
        module = repository_ctx.attr.module,
        version = repository_ctx.attr.version,
    ))

tsgo_source_repo = repository_rule(
    implementation = _tsgo_source_repo_impl,
    attrs = {
        "binary": attr.string(
            doc = "The compiler's go_binary in the module's repository, " +
                  "`@com_github_microsoft_typescript_go//cmd/tsgo:tsgo`.",
            mandatory = True,
        ),
        "module": attr.string(
            doc = "The compiler's module path.",
            mandatory = True,
        ),
        "version": attr.string(
            doc = "The version ts/private/tsgo_source/go.mod requires.",
            mandatory = True,
        ),
    },
    doc = """The source-built compiler behind one label and its pin as Starlark.

`@tsgo_source//:tsgo` aliases the module's `cmd/tsgo` binary, so the toolchain
macro knows nothing of the module's layout; `@tsgo_source//:defs.bzl` carries
`TSGO_SOURCE_MODULE` and `TSGO_SOURCE_VERSION` for //ts/private/tsgo:tsgo_test.
""",
)

def _go_repository_table_impl(repository_ctx):
    rules = []
    for name, importpath in sorted(repository_ctx.attr.importpaths.items()):
        rules.append((
            "go_repository(\n" +
            "    name = \"{}\",\n" +
            "    importpath = \"{}\",\n" +
            ")\n"
        ).format(name, importpath))
    repository_ctx.file("WORKSPACE", "".join(rules))
    repository_ctx.file("BUILD.bazel", "exports_files([\"WORKSPACE\"])\n")

go_repository_table = repository_rule(
    implementation = _go_repository_table_impl,
    attrs = {
        "importpaths": attr.string_dict(
            doc = "Repository name to module path, one entry per module.",
            mandatory = True,
        ),
    },
    doc = """The table Gazelle reads while it writes a module repository's BUILD
files, `go_repository`'s `build_config`: an import of a module in the table
resolves to its repository, and an import of one outside it to nothing.""",
)
