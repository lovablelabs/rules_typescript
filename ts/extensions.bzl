"""Module extension for the rules_typescript toolchains, the linter and the
source-built compiler."""

load("@gazelle//:deps.bzl", "go_repository")
load("//npm:lazy.bzl", "lockfile_registries")
load("//npm/private:npm_translate_lock.bzl", "workspace_registries")
load("//ts/private:go_mod.bzl", "go_repo_name", "parse_go_mod", "tsgo_source_modules")
load(
    "//ts/private:toolchain.bzl",
    "TSGO_PLATFORMS",
    "go_repository_table",
    "tools_toolchain_repo",
    "tsgo_source_repo",
    "tsgo_toolchain_repo",
)
load(
    "//ts/private:tools_lock.bzl",
    "TOOLS_INTEGRITY",
    "TOOLS_VERSION",
    "tools_asset_prefix",
    "tools_asset_url",
)
load("//ts/private:tsgo_lock.bzl", "tsgo_from_pnpm_lock", "tsgo_from_version")
load("//ts/private/actions:format.bzl", "format_config_repo")
load("//ts/private/actions:check.bzl", "check_config_repo")
load("//ts/private/actions:lint.bzl", "lint_config_repo")

# Label(), not a string, so it resolves in this repository from any consumer.
_DEFAULT_TSGO_LOCK = Label("//ts/private/tsgo:pnpm-lock.yaml")
_TSGO_SOURCE_GO_MOD = Label("//ts/private/tsgo_source:go.mod")
_TSGO_SOURCE_GO_SUM = Label("//ts/private/tsgo_source:go.sum")
_LISTING_GO_MOD = Label("//gazelle/tsgolisting:go.mod")

# Gazelle lists in process through the release's own source: none of the
# compiler's patches, whose listings differ from the release binary's.
_TSGO_RELEASE_REPO = "typescript_go_release"
_TSGO_SOURCE_PATCHES = [
    Label("//ts/private/tsgo_source:isolated-declarations-bound-expando.patch"),
    Label("//ts/private/tsgo_source:module-augmentation-include-reason.patch"),
]

# "<prefix>_<platform>", the labels the declare_*_toolchains() macros generate;
# rules_typescript alone use_repo's them, in its own repo mapping.
_TSGO_REPO_PREFIX = "tsgo"
_TOOLS_REPO_PREFIX = "tools"

def _from_lock(module_ctx, pnpm_lock, package, npmrc):
    return tsgo_from_pnpm_lock(
        module_ctx.read(pnpm_lock),
        package,
        TSGO_PLATFORMS,
        str(pnpm_lock),
        lockfile_registries(module_ctx, pnpm_lock, npmrc),
    )

def _spec_for(module_ctx, tag):
    if tag == None:
        return _from_lock(module_ctx, _DEFAULT_TSGO_LOCK, "typescript", None)
    if tag.pnpm_lock and tag.version:
        fail("ts.tsgo(): set pnpm_lock or version, not both. pnpm_lock reads the version and the integrity of every download from the lockfile; version names a release and downloads it unverified.")
    if tag.pnpm_lock:
        return _from_lock(module_ctx, tag.pnpm_lock, tag.package, tag.npmrc)
    if tag.version:
        npmrc = module_ctx.read(tag.npmrc) if tag.npmrc else ""
        return tsgo_from_version(
            tag.package,
            tag.version,
            TSGO_PLATFORMS,
            workspace_registries(npmrc, ""),
        )
    fail("ts.tsgo(): set pnpm_lock (the lockfile whose TypeScript the toolchain fetches, verified) or version (a release to download unverified).")

def _check_for(module_ctx):
    for mod in module_ctx.modules:
        if mod.is_root:
            if len(mod.tags.check) > 1:
                fail(("ts.check(): the root module calls it {} times; one " +
                      "call names the checker every target runs.").format(
                    len(mod.tags.check),
                ))
            for tag in mod.tags.check:
                return tag
    return None

def _lint_for(module_ctx):
    for mod in module_ctx.modules:
        if mod.is_root:
            if len(mod.tags.lint) > 1:
                fail(("ts.lint(): the root module calls it {} times; one " +
                      "call names the linter every target runs.").format(
                    len(mod.tags.lint),
                ))
            for tag in mod.tags.lint:
                return tag
    return None

def _tsgo_source(module_ctx):
    source = tsgo_source_modules(
        module_ctx.read(_TSGO_SOURCE_GO_MOD),
        module_ctx.read(_TSGO_SOURCE_GO_SUM),
        str(_TSGO_SOURCE_GO_MOD),
    )
    if source.error:
        fail(source.error)
    go_repository_table(
        name = "tsgo_source_modules",
        importpaths = {go_repo_name(m.path): m.path for m in source.modules},
    )
    tool = source.tool
    for module in source.modules:
        patches = _TSGO_SOURCE_PATCHES if module.path == tool.module else []

        # Gazelle links typescript-go in, beside whatever copy of golang.org/x/sync
        # and the like a binary's other languages bring: under their own importmap
        # the two link as separate packages instead of conflicting.
        directives = [] if module.path == tool.module else [
            "gazelle:importmap_prefix rules_typescript_tsgo/" + module.path,
        ]
        go_repository(
            name = go_repo_name(module.path),
            build_config = "@tsgo_source_modules//:WORKSPACE",
            build_directives = directives,
            build_file_generation = "on",
            importpath = module.path,
            patch_args = ["-p1"],
            patches = patches,
            sum = module.sum,
            version = module.version,
        )
    listed = parse_go_mod(module_ctx.read(_LISTING_GO_MOD)).requires.get(tool.module)
    if listed != tool.version:
        fail("{} requires {} {} and {} requires {}; the two go.mod files pin one typescript-go.".format(
            _LISTING_GO_MOD,
            tool.module,
            listed,
            _TSGO_SOURCE_GO_MOD,
            tool.version,
        ))
    release = [m for m in source.modules if m.path == tool.module][0]
    go_repository(
        name = _TSGO_RELEASE_REPO,
        build_config = "@tsgo_source_modules//:WORKSPACE",
        build_directives = ["gazelle:go_visibility //visibility:public"],
        build_file_generation = "on",
        importpath = release.path,
        sum = release.sum,
        version = release.version,
    )
    tsgo_source_repo(
        name = "tsgo_source",
        binary = "@{}//{}:{}".format(
            go_repo_name(tool.module),
            tool.package[len(tool.module):].lstrip("/"),
            tool.package.rsplit("/", 1)[-1],
        ),
        module = tool.module,
        version = tool.version,
    )

def _prebuilt_toolchains_repo_impl(rctx):
    rctx.file("BUILD.bazel", """load(%r, "declare_tools_toolchains", "declare_launcher_toolchains")
package(default_visibility = ["//visibility:public"])
declare_tools_toolchains(name = "tools", repo_prefix = "tools")
declare_launcher_toolchains(name = "launcher", repo_prefix = "tools")
""" % str(rctx.attr._defs))

_prebuilt_toolchains_repo = repository_rule(
    implementation = _prebuilt_toolchains_repo_impl,
    attrs = {"_defs": attr.label(default = Label("//ts/private:toolchain.bzl"))},
)

def _ts_impl(module_ctx):
    tag = None
    for mod in module_ctx.modules:
        if mod.is_root:
            for candidate in mod.tags.tsgo:
                tag = candidate

    spec = _spec_for(module_ctx, tag)
    if spec.error:
        fail(spec.error)

    check = _check_for(module_ctx)
    check_config_repo(
        name = "check_config",
        binary = str(check.binary) if check else "",
        args = check.args if check else [],
    )

    lint = _lint_for(module_ctx)
    lint_config_repo(
        name = "lint_config",
        binary = str(lint.binary) if lint else "",
        config = str(lint.config) if lint and lint.config else "",
        fail_on_warnings = lint.fail_on_warnings if lint else False,
        data = [str(label) for label in lint.data] if lint else [],
        args = lint.args if lint else [],
        tool_env = {str(label): name for label, name in lint.tool_env.items()} if lint else {},
    )

    for platform in TSGO_PLATFORMS:
        resolved = spec.platforms[platform]
        tsgo_toolchain_repo(
            name = "{}_{}".format(_TSGO_REPO_PREFIX, platform),
            package = resolved.package,
            url = resolved.url,
            integrity = resolved.integrity,
            binary = spec.binary,
            npmrc = tag.npmrc if tag != None else None,
        )
    prebuilt = []
    for mod in module_ctx.modules:
        if mod.is_root:
            prebuilt = mod.tags.prebuilt_tools
    if len(prebuilt) > 1:
        fail("ts.prebuilt_tools(): call once in the root module")
    if prebuilt:
        for platform in TSGO_PLATFORMS:
            tools_toolchain_repo(
                name = "{}_{}".format(_TOOLS_REPO_PREFIX, platform),
                url = tools_asset_url(TOOLS_VERSION, platform),
                integrity = TOOLS_INTEGRITY[platform],
                strip_prefix = tools_asset_prefix(TOOLS_VERSION, platform),
            )
        _prebuilt_toolchains_repo(name = "tools_prebuilt")
    formatter = None
    for mod in module_ctx.modules:
        if mod.is_root:
            if len(mod.tags.format) > 1:
                fail("ts.format(): call once in the root MODULE.bazel")
            formatter = mod.tags.format[0] if mod.tags.format else None
    format_config_repo(
        name = "format_config",
        binary = str(formatter.binary) if formatter else "",
        config = str(formatter.config) if formatter and formatter.config else "",
        data = [str(label) for label in formatter.data] if formatter else [],
        args = formatter.args if formatter else [],
    )
    _tsgo_source(module_ctx)

    # Only ts.tsgo(version = ...) downloads a compiler no integrity pins, and
    # then MODULE.bazel.lock is the one record of the bytes the build used.
    return module_ctx.extension_metadata(
        reproducible = all([spec.platforms[p].integrity for p in TSGO_PLATFORMS]),
    )

_tsgo_tag = tag_class(
    attrs = {
        "pnpm_lock": attr.label(
            allow_single_file = True,
            doc = """The pnpm-lock.yaml whose TypeScript the toolchain fetches.

The root importer's entry for `package` names the version, and the lockfile's
`packages:` entries for its platform packages carry the tarball and the
integrity of each compiler binary, which Bazel verifies. A `pnpm install` that
moves the version moves the toolchain.""",
        ),
        "package": attr.string(
            default = "typescript",
            doc = """The npm package the compiler comes from: `typescript` (a TypeScript 7
release, whose binary is `tsc`) or `@typescript/native-preview` (a nightly, whose
binary is `tsgo`).""",
        ),
        "version": attr.string(
            doc = """A release of `package` to download from the registry, unverified.

The alternative to `pnpm_lock` for a build no lockfile describes: a consumer
without pnpm, or a nightly being bisected. Nothing checks the bytes.""",
        ),
        "npmrc": attr.label(
            allow_single_file = True,
            doc = """The workspace .npmrc, when it names a registry or a
credential.

`registry=` and `@typescript:registry=` decide the tarball host, with the
`registries:` of the pnpm-workspace.yaml beside `pnpm_lock`, and
`//host/:_authToken=` or `PNPM_CONFIG__AUTH` its credentials, as for
npm.translate_lock; the token is read at fetch time and never becomes an
attribute value.""",
        ),
    },
    doc = """Which TypeScript compiler the tsgo toolchain runs.

Only the root module's ts.tsgo() call takes effect.  Transitive dependencies
that also call ts.tsgo() are ignored.  With no call at all the toolchain is the
`typescript` release rules_typescript's own ts/private/tsgo/pnpm-lock.yaml pins.

Example:
    ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
    ts.tsgo(pnpm_lock = "//:pnpm-lock.yaml")

The unverified alternative, for a version no lockfile states:
    ts.tsgo(version = "7.0.2")
    ts.tsgo(package = "@typescript/native-preview", version = "7.0.0-dev.20260311.1")
""",
)

_format_tag = tag_class(
    attrs = {
        "binary": attr.label(mandatory = True),
        "config": attr.label(allow_single_file = True),
        "data": attr.label_list(allow_files = True),
        "args": attr.string_list(default = ["--check"]),
    },
    doc = "Read-only formatting validation on checked-in program sources and ts_format file sets; only the root module configures it.",
)

_check_tag = tag_class(
    attrs = {
        "binary": attr.label(
            mandatory = True,
            doc = """The type checker TsgoCheck runs instead of the tsgo
toolchain's compiler. It is called with `args` and then tsgo's command line
(`--project`, `--noEmit`, `--explainFiles`, `--pretty false`, `--checkers`),
and has to print and exit as tsgo does. Declaration emit, the editor project
and Gazelle stay on tsgo.""",
        ),
        "args": attr.string_list(
            doc = "Arguments before tsgo's command line: `[\"tsc\"]` for `oj tsc`.",
        ),
    },
    doc = """Which type checker TsgoCheck runs; only the root module's call
takes effect.

    ts.check(binary = "@oj//:oj", args = ["tsc"])
""",
)

_lint_tag = tag_class(
    attrs = {
        "binary": attr.label(
            mandatory = True,
            doc = """The linter's executable: `@npm//:oxlint_bin` or
`@npm//:eslint_bin` from the workspace's hub, so the lockfile decides which
oxlint or eslint runs. There is no linter toolchain.""",
        ),
        "config": attr.label(
            allow_single_file = True,
            doc = """The linter's own config file, passed as `--config`. Unset,
no flag is passed: oxlint runs its defaults, and eslint needs a flat config.
Another repository names the file, so its package exports it
(`exports_files`).""",
        ),
        "tool_env": attr.label_keyed_string_dict(
            doc = "Executable labels mapped to environment names receiving their absolute paths.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Config imports, plugins and ignore files available at their workspace paths.",
        ),
        "args": attr.string_list(
            doc = "Linter arguments. {tsconfig} expands to the target-specific compiler config.",
        ),
        "fail_on_warnings": attr.bool(
            doc = "A warning fails the build: `--max-warnings=0`, which " +
                  "oxlint and eslint spell alike.",
        ),
    },
    doc = """The linter every ts_compile and ts_test runs over its sources as a
validation action (docs/guides/lint.md).

Only the root module's ts.lint() call takes effect, and it makes one; a module
graph with no call lints nothing. The call writes the @lint_config repository,
whose one target the //ts:lint label flag names.

Example:
    ts = use_extension("@rules_typescript//ts:extensions.bzl", "ts")
    ts.lint(binary = "@npm//:oxlint_bin", config = "//:oxlint.json")
""",
)

# Deliberately neither os_dependent nor arch_dependent: it declares a repo for
# every supported platform and reads nothing about the host, so its result --
# and MODULE.bazel.lock -- is identical everywhere.  Only the repo Bazel
# actually fetches depends on where the build runs.
ts = module_extension(
    implementation = _ts_impl,
    tag_classes = {
        "tsgo": _tsgo_tag,
        "check": _check_tag,
        "lint": _lint_tag,
        "format": _format_tag,
        "prebuilt_tools": tag_class(attrs = {}, doc = "Opt into the pinned Go tools release from the root module."),
    },
)
