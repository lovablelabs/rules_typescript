"""Module extension for npm dependency management.

Usage in MODULE.bazel:
    npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
    npm.pnpm(version = "10.34.9")          # optional; defaults to _DEFAULT_PNPM_VERSION
    npm.translate_lock(pnpm_lock = "//:pnpm-lock.yaml")
    use_repo(npm, "npm", "pnpm")

Design note: when both the root workspace and rules_typescript register a repo
with the same name (e.g. "npm"), the root workspace wins.  This lets consumers
provide their own pnpm-lock.yaml while rules_typescript ships a default lockfile
for its own tests.  Non-root registrations for a name are silently skipped when
the root module has already claimed that name. A non-root module's hub that
does fill in serves that module's own targets (`@npm_esbuild` for
`//vite:vite_plugin_bazel`): its store sits in that module's lockfile package,
and a link never crosses a repository (docs/rules/node-modules.md § The Store).
"""

load("//npm:lazy.bzl", "declare_lazy_npm_repos")
load("//ts/private:pnpm.bzl", "DEFAULT_PNPM_VERSION", "pnpm_repo")

def _npm_impl(module_ctx):
    # Collect registrations in two passes:
    #   1. Root-module registrations take priority.
    #   2. Non-root registrations only fill in names not already claimed.
    claimed = {}  # name → the translate_lock tag that owns it

    # Pass 1: root module.
    for mod in module_ctx.modules:
        if not mod.is_root:
            continue
        for lock_tag in mod.tags.translate_lock:
            if lock_tag.name not in claimed:
                claimed[lock_tag.name] = lock_tag

    # Pass 2: non-root modules (fill in unclaimed names only).
    for mod in module_ctx.modules:
        if mod.is_root:
            continue
        for lock_tag in mod.tags.translate_lock:
            if lock_tag.name not in claimed:
                claimed[lock_tag.name] = lock_tag

    lockfile_of = {}
    for name, lock_tag in claimed.items():
        lock = lock_tag.pnpm_lock
        package = "@@{}//{}".format(lock.repo_name, lock.package)
        if package in lockfile_of:
            fail(
                "npm: {} and {} name lockfiles in one package, {}: ".format(
                    lockfile_of[package],
                    lock,
                    package,
                ) + "two lockfiles in one package would share one store " +
                "(node_modules/.pnpm). Move one into a package of its own.",
            )
        lockfile_of[package] = lock_tag.pnpm_lock

    for name, lock_tag in claimed.items():
        declare_lazy_npm_repos(
            module_ctx,
            name,
            lock_tag.pnpm_lock,
            lock_tag.patches,
            lock_tag.npmrc,
        )

    # ── pnpm hermetic binary ──────────────────────────────────────────────────
    # The root module's npm.pnpm(version=...) tag sets the version; other
    # modules are ignored.  When no npm.pnpm() tag is present at all, we still
    # download the default version so that rules_typescript's own tests have a
    # hermetic pnpm available.
    pnpm_version = DEFAULT_PNPM_VERSION
    pnpm_repo_name = "pnpm"

    for mod in module_ctx.modules:
        if not mod.is_root:
            continue
        for tag in mod.tags.pnpm:
            pnpm_version = tag.version
            if tag.name:
                pnpm_repo_name = tag.name

    pnpm_repo(
        name = pnpm_repo_name,
        version = pnpm_version,
    )

    return module_ctx.extension_metadata(reproducible = True)

_translate_lock_tag = tag_class(attrs = {
    "name": attr.string(
        default = "npm",
        doc = """The alias hub's repository name, which is also what use_repo takes and what
BUILD labels spell (`@npm//:react`).

One call per lockfile: a workspace translating several gets one hub each;
Gazelle writes labels into `@npm`, the root lockfile's hub, and a target that
resolves into another hub is hand-written. Only the root module's
registration for a name takes effect; a non-root module's is skipped, which is
what lets a consumer supply its own lockfile under the name rules_typescript uses
for its own tests.""",
    ),
    "pnpm_lock": attr.label(mandatory = True, allow_single_file = True),
    "npmrc": attr.label(
        allow_single_file = True,
        doc = """The workspace .npmrc, when it names a registry or a credential.

A pnpm lockfile records name@version and integrity and no registry. The
extension reads the registry map the way pnpm does: this file's `registry=` and
`@scope:registry=` lines, then the `registries:` block and `registry:` of the
pnpm-workspace.yaml beside the lockfile, which needs no label because pnpm keeps
it there. A workspace whose registries are all in pnpm-workspace.yaml passes
nothing here:

    npm.translate_lock(
        pnpm_lock = "//:pnpm-lock.yaml",
        npmrc = "//:.npmrc",
    )

Credentials (`//host/:_authToken=`, `//host/:_auth=`) are NOT read by the
extension: each package's own fetch reads them, and pnpm's `auth` setting,
`PNPM_CONFIG__AUTH`, from its environment, because the extension's result is
written to MODULE.bazel.lock, which is committed. `${VAR}` in this file is
resolved at fetch time from the environment.

`~/.npmrc` is deliberately not consulted. Bazel cannot make a file outside the
workspace an input, so reading it would mean one lockfile and one lock fetching
different bytes on two machines.""",
    ),
    "patches": attr.label_list(
        allow_files = True,
        doc = """The patch files named by the lockfile's `patchedDependencies`.

pnpm keeps the patch paths in pnpm-workspace.yaml, which the extension cannot
turn into labels: a path like `patches/foo.patch` says nothing about where the
consumer's Bazel package boundaries fall. So pass the files as labels and the
extension pairs each one with its lockfile entry by filename, which is pnpm's
own naming (`<name with / replaced by __>@<version>.patch`):

    npm.translate_lock(
        pnpm_lock = "//:pnpm-lock.yaml",
        patches = ["//patches:@acme__diffs@1.3.1.patch"],
    )

Each label is then resolved to a file and checked against the sha256 the
lockfile records beside the entry, so a label that names nothing readable, or a
file pnpm never saw, fails here rather than wherever the patch mattered. So does
an entry with no matching file, and a file no entry claims.

A scoped package's patch keeps the leading '@' of its name, and a target name
starting with '@' cannot come out of `glob()` -- glob prefixes ':' onto such a
result and `exports_files` rejects it, failing the whole package. List those
files literally.""",
    ),
})

_pnpm_tag = tag_class(
    attrs = {
        "name": attr.string(
            default = "pnpm",
            doc = "Name of the external repository that will contain the pnpm binary (default 'pnpm').",
        ),
        "version": attr.string(
            default = DEFAULT_PNPM_VERSION,
            doc = "pnpm version to download (e.g. '10.34.9'). Defaults to the bundled stable version.",
        ),
    },
    doc = """\
Pin the hermetic pnpm version used by this workspace.

Only the root module's npm.pnpm() call takes effect.  Transitive dependencies
that also call npm.pnpm() are ignored.

When this tag is absent, rules_typescript downloads the built-in default
pnpm version automatically, so no explicit call is needed unless you want to
override the version.

Example:
    npm = use_extension("@rules_typescript//npm:extensions.bzl", "npm")
    npm.pnpm(version = "10.34.9")
    use_repo(npm, "pnpm")
""",
)

npm = module_extension(
    implementation = _npm_impl,
    tag_classes = {
        "translate_lock": _translate_lock_tag,
        "pnpm": _pnpm_tag,
    },
)
