"""Unit tests for the pnpm lockfile reader.

Most of what these pin is a NON-feature. catalogs, overrides and
packageExtensions need no implementation at all: pnpm resolves every one of them
before it writes the lockfile, so the resolved graph already carries the answer
and re-deriving it would mean redoing pnpm's work with worse information.

That makes them fragile in a specific way. Each of those blocks is shaped like a
block we do read, and each sits at indent 0 next to the ones we do read, so the
only thing keeping them out is that every reader anchors on its own section
header and stops at the next indent-0 line. A reader rewritten to scan the file
for `specifier:`/`version:` pairs, or for `name@version` keys, would start
silently pulling in catalog entries and override selectors. These tests fail if
that happens.
"""

load("@bazel_skylib//lib:partial.bzl", "partial")
load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//npm:lazy.bzl", "break_cycles", "patch_file_name")
load("//npm/private:fetch_auth.bzl", "fetch_auth")
load(
    "//npm/private:npm_translate_lock.bzl",
    "npm_tarball_url",
    "npmrc_registries",
    "parse_importers",
    "parse_patched_dependencies",
    "parse_pnpm_lock",
    "peer_suffix_dir_name",
    "pnpm_workspace_registries",
    "verify_integrity",
    "workspace_registries",
)

# A lockfile with one of every block, in the order pnpm writes them.
#
# The catalog is the adversarial part: its entries carry a `link:` version and an
# npm-alias version, so a reader that scanned globally instead of anchoring would
# report a workspace member and an alias that do not exist. `overrides:` is
# adversarial the same way for the packages reader -- `lodash@4` and
# `pkg>lodash` look like package keys.
_LOCKFILE = """lockfileVersion: '9.0'

settings:
  autoInstallPeers: true

catalogs:
  default:
    catalog-linked:
      specifier: workspace:*
      version: link:packages/catalog-only
    catalog-aliased:
      specifier: npm:lodash@9.9.9
      version: lodash@9.9.9
  ts6:
    typescript:
      specifier: npm:@typescript/typescript6@6.0.2
      version: 6.0.2

overrides:
  lodash@4: 4.18.1
  '@acme/toolkit>esbuild': 0.28.1

patchedDependencies:
  '@acme/diffs@1.3.1': 1111111111111111111111111111111111111111111111111111111111111111
  acme-editor@1.41.4:
    hash: 2222222222222222222222222222222222222222222222222222222222222222
    path: patches/acme-editor@1.41.4.patch

importers:

  .:
    dependencies:
      shared:
        specifier: workspace:*
        version: link:packages/shared
      tw-v3:
        specifier: npm:tailwindcss@3.4.18
        version: tailwindcss@3.4.18(tsx@4.23.12)
      typescript:
        specifier: npm:@typescript/typescript6@6.0.2
        version: '@typescript/typescript6@6.0.2'
      react-dom:
        specifier: 19.2.4
        version: 19.2.4(react@19.2.4)
    devDependencies:
      lodash:
        specifier: 4.18.1
        version: 4.18.1

  # An importer with no dependencies: pnpm writes it inline.
  packages/leaf: {}

  packages/nested:
    dependencies:
      up-one:
        specifier: workspace:*
        version: link:../other

packages:

  '@dub/analytics@0.0.32':
    resolution: {integrity: sha512-injected==}
    peerDependencies:
      react: 19.2.4

  '@oxlint/linux-x64-gnu@0.16.6':
    resolution: {integrity: sha512-gnu==}
    cpu: [x64]
    os: [linux]
    libc: [glibc]

  '@oxlint/linux-x64-musl@0.16.6':
    resolution: {integrity: sha512-musl==}
    cpu: [x64]
    os: [linux]
    libc: [musl]

  esbuild@0.27.3:
    resolution: {integrity: sha512-old==}

  esbuild@0.28.1:
    resolution: {integrity: sha512-new==}

  lodash@4.18.1:
    resolution: {integrity: sha512-lodash==}

  react@19.2.4:
    resolution: {integrity: sha512-react==}

snapshots:

  '@dub/analytics@0.0.32(react@19.2.4)':
    dependencies:
      react: 19.2.4

  '@oxlint/linux-x64-gnu@0.16.6': {}

  '@oxlint/linux-x64-musl@0.16.6': {}

  esbuild@0.27.3: {}

  esbuild@0.28.1: {}

  lodash@4.18.1: {}

  react@19.2.4: {}
"""

def _catalogs_are_not_importers_test(ctx):
    env = unittest.begin(ctx)
    importers = parse_importers(_LOCKFILE)

    asserts.equals(env, {
        "shared": "packages/shared",
        "up-one": "packages/other",
    }, importers["links"])
    asserts.equals(env, {
        "tw-v3": "tailwindcss@3.4.18(tsx@4.23.12)",
        "typescript": "@typescript/typescript6@6.0.2",
    }, importers["aliases"])

    # Stated on their own because they are the whole point: these two live in
    # `catalogs:`, and pnpm has already substituted a concrete version at every
    # place they are used.
    asserts.false(env, "catalog-linked" in importers["links"])
    asserts.false(env, "catalog-aliased" in importers["aliases"])

    return unittest.end(env)

catalogs_are_not_importers_test = unittest.make(_catalogs_are_not_importers_test)

def _alias_values_are_snapshot_ids_test(ctx):
    env = unittest.begin(ctx)
    importers = parse_importers(_LOCKFILE)

    # The hub looks an alias up in the resolved graph, which is keyed by the
    # peer-decorated snapshot id. A peer-stripped value is a key of `packages:`,
    # never of `snapshots:`, so it matches nothing and the alias gets no target
    # at all -- while the same name resolved without peers gets one.
    root_deps = importers["importers"]["."]["deps"]
    asserts.true(env, len(importers["aliases"]) > 0)
    for name, sid in importers["aliases"].items():
        asserts.equals(env, root_deps[name], sid)

    return unittest.end(env)

alias_values_are_snapshot_ids_test = unittest.make(_alias_values_are_snapshot_ids_test)

def _catalogs_and_overrides_are_not_packages_test(ctx):
    env = unittest.begin(ctx)
    packages = parse_pnpm_lock(_LOCKFILE)["packages"]

    asserts.equals(env, [
        "@dub/analytics@0.0.32",
        "@oxlint/linux-x64-gnu@0.16.6",
        "@oxlint/linux-x64-musl@0.16.6",
        "esbuild@0.27.3",
        "esbuild@0.28.1",
        "lodash@4.18.1",
        "react@19.2.4",
    ], sorted(packages.keys()))

    return unittest.end(env)

catalogs_and_overrides_are_not_packages_test = unittest.make(_catalogs_and_overrides_are_not_packages_test)

_FILE_TARBALL_LOCK = """lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@scope/vendored':
        specifier: file:vendor/vendored-3.2.1.tgz
        version: file:vendor/vendored-3.2.1.tgz

packages:

  '@scope/vendored@file:vendor/vendored-3.2.1.tgz':
    resolution: {integrity: sha512-aaa, tarball: file:vendor/vendored-3.2.1.tgz}
    version: 3.2.1

snapshots:

  '@scope/vendored@file:vendor/vendored-3.2.1.tgz': {}
"""

# An inline `{}` snapshot key cut at the colon in `file:` dropped the package
# from the graph, and its path ranked as version 0.0.0 against registry copies.
def _file_tarball_snapshot_keeps_key_and_version_test(ctx):
    env = unittest.begin(ctx)
    key = "@scope/vendored@file:vendor/vendored-3.2.1.tgz"
    snapshots = parse_pnpm_lock(_FILE_TARBALL_LOCK)["snapshots"]

    asserts.equals(env, [key], snapshots.keys())
    asserts.equals(env, "3.2.1", snapshots[key].get("semver"))

    return unittest.end(env)

file_tarball_snapshot_keeps_key_and_version_test = unittest.make(_file_tarball_snapshot_keeps_key_and_version_test)

def _platform_constraints_are_read_test(ctx):
    env = unittest.begin(ctx)
    packages = parse_pnpm_lock(_LOCKFILE)["packages"]

    gnu = packages["@oxlint/linux-x64-gnu@0.16.6"]
    musl = packages["@oxlint/linux-x64-musl@0.16.6"]
    asserts.equals(env, ["linux"], gnu["os"])
    asserts.equals(env, ["x64"], gnu["cpu"])
    asserts.equals(env, ["glibc"], gnu.get("libc"))
    asserts.equals(env, ["musl"], musl.get("libc"))

    asserts.equals(env, [], packages["lodash@4.18.1"].get("libc"))

    return unittest.end(env)

platform_constraints_are_read_test = unittest.make(_platform_constraints_are_read_test)

def _overrides_arrive_pre_resolved_test(ctx):
    env = unittest.begin(ctx)
    packages = parse_pnpm_lock(_LOCKFILE)["packages"]

    # A global override is a version SELECTION, so its outcome is the only
    # version present: nothing has to resolve `lodash@4` -> 4.18.1.
    asserts.equals(env, "4.18.1", packages["lodash@4.18.1"]["version"])

    # A package-scoped override (`@acme/toolkit>esbuild`) materialises as
    # a second version rather than a second resolution of one version, so the
    # name@version keyed map represents it with no parent>child machinery.
    asserts.equals(env, "0.27.3", packages["esbuild@0.27.3"]["version"])
    asserts.equals(env, "0.28.1", packages["esbuild@0.28.1"]["version"])

    return unittest.end(env)

overrides_arrive_pre_resolved_test = unittest.make(_overrides_arrive_pre_resolved_test)

def _package_extensions_arrive_as_edges_test(ctx):
    env = unittest.begin(ctx)
    parsed = parse_pnpm_lock(_LOCKFILE)

    # @dub/analytics declares no peer upstream; the react peer exists only
    # because of a packageExtension. pnpm writes it into packages: as a peer
    # range and into snapshots: as a resolved edge, and the resolved edge is the
    # one that makes it a dependency here.
    asserts.equals(env, {"react": "19.2.4"}, parsed["packages"]["@dub/analytics@0.0.32"]["peerDependencies"])
    asserts.equals(
        env,
        {"react": "react@19.2.4"},
        parsed["snapshots"]["@dub/analytics@0.0.32(react@19.2.4)"]["dependencies"],
    )

    return unittest.end(env)

package_extensions_arrive_as_edges_test = unittest.make(_package_extensions_arrive_as_edges_test)

# The same package, resolved twice. This is not an edge case pnpm tolerates: it
# is how pnpm satisfies a peer range that two dependents pin differently, and it
# is the only record of the distinction anywhere in the lockfile. `packages:` has
# one entry -- one tarball -- and says nothing about it.
_PEER_LOCKFILE = """lockfileVersion: '9.0'

importers:

  apps/legacy:
    dependencies:
      styled:
        specifier: ^8.0.0
        version: 8.6.14(react@18.3.1)
      react:
        specifier: ^18.0.0
        version: 18.3.1

  apps/next:
    dependencies:
      styled:
        specifier: ^8.0.0
        version: 8.6.14(react@19.0.0)
      react:
        specifier: ^19.0.0
        version: 19.0.0
      tw:
        specifier: npm:tailwindcss@3.4.18
        version: tailwindcss@3.4.18

packages:

  react@18.3.1:
    resolution: {integrity: sha512-r18==}

  react@19.0.0:
    resolution: {integrity: sha512-r19==}

  styled@8.6.14:
    resolution: {integrity: sha512-styled==}
    peerDependencies:
      react: ^18 || ^19

  tailwindcss@3.4.18:
    resolution: {integrity: sha512-tw==}

snapshots:

  react@18.3.1: {}

  react@19.0.0: {}

  styled@8.6.14(react@18.3.1):
    dependencies:
      react: 18.3.1

  styled@8.6.14(react@19.0.0):
    dependencies:
      react: 19.0.0

  tailwindcss@3.4.18: {}
"""

def _snapshots_keep_peer_variants_test(ctx):
    env = unittest.begin(ctx)
    parsed = parse_pnpm_lock(_PEER_LOCKFILE)

    # One tarball...
    asserts.equals(env, ["react@18.3.1", "react@19.0.0", "styled@8.6.14", "tailwindcss@3.4.18"], sorted(parsed["packages"].keys()))

    # ...two resolutions, and the whole point is that their dependency edges
    # differ. Keying the graph by the packages: key merges these into one dict,
    # which is a wrong answer nothing downstream can detect: 18.3.1 and 19.0.0
    # are both real versions of a package that really is a dependency.
    asserts.equals(env, [
        "react@18.3.1",
        "react@19.0.0",
        "styled@8.6.14(react@18.3.1)",
        "styled@8.6.14(react@19.0.0)",
        "tailwindcss@3.4.18",
    ], sorted(parsed["snapshots"].keys()))

    asserts.equals(
        env,
        {"react": "react@18.3.1"},
        parsed["snapshots"]["styled@8.6.14(react@18.3.1)"]["dependencies"],
    )
    asserts.equals(
        env,
        {"react": "react@19.0.0"},
        parsed["snapshots"]["styled@8.6.14(react@19.0.0)"]["dependencies"],
    )

    # Both resolutions name the one tarball they share, so the download is still
    # declared once per name@version.
    asserts.equals(env, "styled@8.6.14", parsed["snapshots"]["styled@8.6.14(react@18.3.1)"]["package_id"])
    asserts.equals(env, "styled@8.6.14", parsed["snapshots"]["styled@8.6.14(react@19.0.0)"]["package_id"])

    return unittest.end(env)

snapshots_keep_peer_variants_test = unittest.make(_snapshots_keep_peer_variants_test)

_DIR_UNSAFE = "()@/.,'\"+ "

def _peer_tokens_separate_peer_sets_test(ctx):
    env = unittest.begin(ctx)

    # The token names one peer set, and it names the store directory a
    # non-primary resolution's files go in as well as the repository they come
    # from. So it has to be a filename, and it has to be different for peer sets
    # that are different -- including two that agree for their first hundred
    # characters, which nested peer sets routinely do. A token that collided
    # would merge two resolutions into one directory, which is the whole bug.
    asserts.equals(env, "", peer_suffix_dir_name(""))

    long_a = "(react@18.3.1)(react-dom@18.3.1)(typescript@5.9.3)(vite@6.4.1)(zod@3.24.2)"
    long_b = "(react@18.3.1)(react-dom@18.3.1)(typescript@5.9.3)(vite@6.4.1)(zod@4.1.13)"
    tokens = [
        peer_suffix_dir_name(suffix)
        for suffix in ("(ansi-regex@5.0.1)", "(ansi-regex@6.2.2)", long_a, long_b)
    ]
    asserts.equals(env, 4, len(({token: True for token in tokens}).keys()))

    for token in tokens:
        for ch in _DIR_UNSAFE.elems():
            asserts.true(
                env,
                ch not in token,
                "peer token {} holds {}, which is not usable in a path component".format(token, ch),
            )

    return unittest.end(env)

peer_tokens_separate_peer_sets_test = unittest.make(_peer_tokens_separate_peer_sets_test)

def _importers_resolve_per_importer_test(ctx):
    env = unittest.begin(ctx)
    importers = parse_importers(_PEER_LOCKFILE)["importers"]

    # Two members, one name, two majors. A single flat label per package name
    # cannot express this, so the per-importer map is the only place it survives.
    asserts.equals(env, {
        "react": "react@18.3.1",
        "styled": "styled@8.6.14(react@18.3.1)",
    }, importers["apps/legacy"]["deps"])
    asserts.equals(env, {
        "react": "react@19.0.0",
        "styled": "styled@8.6.14(react@19.0.0)",
        "tw": "tailwindcss@3.4.18",
    }, importers["apps/next"]["deps"])

    return unittest.end(env)

importers_resolve_per_importer_test = unittest.make(_importers_resolve_per_importer_test)

def _importer_links_are_importer_relative_test(ctx):
    env = unittest.begin(ctx)
    importers = parse_importers(_LOCKFILE)["importers"]

    # pnpm writes link: targets relative to the member that declares them, so
    # the per-importer map has to carry them resolved, not verbatim.
    asserts.equals(env, {"shared": "packages/shared"}, importers["."]["links"])
    asserts.equals(env, {"up-one": "packages/other"}, importers["packages/nested"]["links"])

    return unittest.end(env)

importer_links_are_importer_relative_test = unittest.make(_importer_links_are_importer_relative_test)

def _inline_importer_test(ctx):
    env = unittest.begin(ctx)
    importers = parse_importers(_LOCKFILE)["importers"]

    # `packages/leaf: {}` is how pnpm writes an importer that declares nothing;
    # the indented comment above it, colon and all, is not an importer.
    asserts.equals(
        env,
        {"deps": {}, "links": {}},
        importers.get("packages/leaf"),
    )
    asserts.equals(
        env,
        [".", "packages/leaf", "packages/nested"],
        sorted(importers.keys()),
    )

    return unittest.end(env)

inline_importer_test = unittest.make(_inline_importer_test)

# The .npmrc half of the registry map. It also holds credentials, which is why
# the two are read in different places.
_NPMRC = """; the workspace default
registry=https://npm.example.com/artifactory/api/npm/npm-virtual/

@acme:registry = https://npm.acme.test/
# a comment that is not an assignment
[an ini section header npm does not use]
always-auth=true

//npm.example.com/:_authToken=host-wide-token
//npm.example.com/artifactory/api/npm/npm-virtual/:_authToken=${NPM_VIRTUAL_TOKEN}
//npm.acme.test/:_auth=YWNtZTpodW50ZXIy
"""

def _npmrc_registries_test(ctx):
    env = unittest.begin(ctx)
    registries = npmrc_registries(_NPMRC)

    # Trailing slash dropped so URL assembly does not double it, and the scope key
    # keeps its '@' because that is how a package name carries it.
    asserts.equals(env, {
        "": "https://npm.example.com/artifactory/api/npm/npm-virtual",
        "@acme": "https://npm.acme.test",
    }, registries)

    # The extension puts these into repository rule attributes, which Bazel writes
    # into MODULE.bazel.lock. Nothing that reads like a credential may be here.
    for value in registries.values():
        asserts.false(env, "token" in value)
        asserts.false(env, "_auth" in value)

    asserts.equals(env, {}, npmrc_registries("always-auth=true\n"))

    return unittest.end(env)

npmrc_registries_test = unittest.make(_npmrc_registries_test)

def _npmrc_registry_picks_the_url_test(ctx):
    env = unittest.begin(ctx)
    registries = npmrc_registries(_NPMRC)

    asserts.equals(
        env,
        "https://npm.acme.test/@acme/widget/-/widget-1.2.3.tgz",
        npm_tarball_url("@acme/widget", "1.2.3", {}, registries),
    )

    # A scope with no override falls back to the workspace default, not to npmjs.
    asserts.equals(
        env,
        "https://npm.example.com/artifactory/api/npm/npm-virtual/@types/react/-/react-19.0.0.tgz",
        npm_tarball_url("@types/react", "19.0.0", {}, registries),
    )
    asserts.equals(
        env,
        "https://npm.example.com/artifactory/api/npm/npm-virtual/zod/-/zod-3.24.2.tgz",
        npm_tarball_url("zod", "3.24.2", {}, registries),
    )

    # No .npmrc at all is the public registry.
    asserts.equals(
        env,
        "https://registry.npmjs.org/zod/-/zod-3.24.2.tgz",
        npm_tarball_url("zod", "3.24.2", {}, {}),
    )

    # A `tarball:` in the lockfile is a URL pnpm already resolved (a git or http
    # dependency) and outranks every registry setting.
    asserts.equals(
        env,
        "https://codeload.example/tarball/abc",
        npm_tarball_url("zod", "3.24.2", {"tarball": "https://codeload.example/tarball/abc"}, registries),
    )

    return unittest.end(env)

npmrc_registry_picks_the_url_test = unittest.make(_npmrc_registry_picks_the_url_test)

# The pnpm-workspace.yaml half: a trailing comment, a comment line, a `${VAR}`
# value, and the block ends at the next top-level key.
_PNPM_WORKSPACE = """packages:
  - packages/*

registry: https://npm.example.com/mirror/   # the default for everything

registries:
  default: https://npm.example.com/below-registry/
  "@acme": https://npm.acme.test/yaml/
  '@castleio': https://npm.castle.io
  "@env": https://${REGISTRY_HOST}/
  # a comment inside the block
frozenLockfile: true
"""

def _pnpm_workspace_registries_test(ctx):
    env = unittest.begin(ctx)

    # `registry:` sets the default over the block's `default`; a value naming
    # `${VAR}` is dropped, as pnpm drops it from a project's file.
    asserts.equals(env, {
        "": "https://npm.example.com/mirror",
        "@acme": "https://npm.acme.test/yaml",
        "@castleio": "https://npm.castle.io",
    }, pnpm_workspace_registries(_PNPM_WORKSPACE))

    asserts.equals(env, {
        "": "https://npm.example.com/blocks",
        "@acme": "https://npm.acme.test",
    }, pnpm_workspace_registries(
        "registries: # private registries\n  default: https://npm.example.com/blocks/\n" +
        "  \"@acme\": https://npm.acme.test/\n",
    ))

    asserts.equals(env, {}, pnpm_workspace_registries("packages:\n  - web\n"))
    asserts.equals(env, {}, pnpm_workspace_registries(""))
    asserts.equals(env, {}, pnpm_workspace_registries("registries:\n"))

    return unittest.end(env)

pnpm_workspace_registries_test = unittest.make(_pnpm_workspace_registries_test)

def _registry_parse_impl(ctx):
    pnpm_workspace_registries(ctx.attr.content)
    return []

_registry_parse = rule(
    implementation = _registry_parse_impl,
    attrs = {"content": attr.string()},
)

def _unsupported_registry_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "use an indented registries mapping")
    return analysistest.end(env)

_unsupported_registry_test = analysistest.make(
    _unsupported_registry_test_impl,
    expect_failure = True,
)

def _workspace_registries_test(ctx):
    env = unittest.begin(ctx)

    # pnpm's order: pnpm-workspace.yaml wins the keys it sets, the .npmrc
    # keeps the rest.
    merged = workspace_registries(_NPMRC, _PNPM_WORKSPACE)
    asserts.equals(env, "https://npm.acme.test/yaml", merged["@acme"])
    asserts.equals(env, "https://npm.example.com/mirror", merged[""])
    asserts.equals(env, "https://npm.castle.io", merged["@castleio"])
    asserts.equals(
        env,
        npmrc_registries(_NPMRC),
        workspace_registries(_NPMRC, ""),
    )

    # A scope mapped by pnpm-workspace.yaml alone, an .npmrc with no registry
    # line: the scoped tarball lives on that registry, the rest on npmjs.
    castle = workspace_registries(
        "# public-hoist-pattern[]=<package-name>\n",
        "registries:\n  \"@castleio\": https://npm.castle.io\n",
    )
    asserts.equals(
        env,
        _CASTLE_JS,
        npm_tarball_url("@castleio/castle-js", "3.2.262440810", {}, castle),
    )
    asserts.equals(
        env,
        "https://registry.npmjs.org/zod/-/zod-3.24.2.tgz",
        npm_tarball_url("zod", "3.24.2", {}, castle),
    )

    return unittest.end(env)

workspace_registries_test = unittest.make(_workspace_registries_test)

_VIRTUAL = "https://npm.example.com/artifactory/api/npm/npm-virtual"
_ELSEWHERE = "https://npm.example.com/elsewhere/zod/-/zod-3.24.2.tgz"
_ACME_WIDGET = "https://npm.acme.test/@acme/widget/-/widget-1.2.3.tgz"
_CASTLE_JS = (
    "https://npm.castle.io/@castleio/castle-js/-/castle-js-3.2.262440810.tgz"
)

def _bearer(token):
    return {
        "type": "pattern",
        "pattern": "Bearer <password>",
        "login": "",
        "password": token,
    }

def _password(auth, url):
    return auth[url]["password"]

def _fetch_auth_test(ctx):
    env = unittest.begin(ctx)

    def getenv(name, default):
        return {"NPM_VIRTUAL_TOKEN": "from-the-environment"}.get(name, default)

    virtual_zod = _VIRTUAL + "/zod/-/zod-3.24.2.tgz"

    # Longest prefix wins: the host-wide token exists too, and taking it would
    # send the wrong credential to a registry mounted on a path.
    asserts.equals(
        env,
        {virtual_zod: _bearer("from-the-environment")},
        fetch_auth(_NPMRC, virtual_zod, "zod", getenv),
    )

    # A path the virtual repo does not cover falls back to the host-wide entry.
    asserts.equals(
        env,
        "host-wide-token",
        _password(fetch_auth(_NPMRC, _ELSEWHERE, "zod", getenv), _ELSEWHERE),
    )

    # `_auth` is already the base64 of user:pass, so it is a Basic header as-is.
    asserts.equals(env, {
        _ACME_WIDGET: {
            "type": "pattern",
            "pattern": "Basic <password>",
            "login": "",
            "password": "YWNtZTpodW50ZXIy",
        },
    }, fetch_auth(_NPMRC, _ACME_WIDGET, "@acme/widget", getenv))

    # An unrelated host gets no header rather than someone else's token.
    asserts.equals(env, {}, fetch_auth(
        _NPMRC,
        "https://registry.npmjs.org/zod/-/zod-3.24.2.tgz",
        "zod",
        getenv,
    ))

    # An unset environment variable must not produce `Bearer ` with nothing after
    # it: an empty token is no token.
    asserts.equals(env, {}, fetch_auth(
        "//npm.example.com/:_authToken=${NOT_SET}\n",
        "https://npm.example.com/zod/-/zod-3.24.2.tgz",
        "zod",
        lambda name, default: default,
    ))

    # pnpm's `auth` setting: JSON keyed by registry URL, then by scope, "@" for
    # every package on that registry.
    setting = json.encode({
        "https://npm.acme.test": {"@acme": {"authToken": "acme-scoped"}},
        _VIRTUAL + "/": {"@": {"authToken": "virtual-from-setting"}},
        "https://npm.castle.io": {"@castleio": {"authToken": "castle-token"}},
    })

    def getenv_setting(name, default):
        return {
            "PNPM_CONFIG__AUTH": setting,
            "NPM_VIRTUAL_TOKEN": "from-the-environment",
        }.get(name, default)

    def getenv_lower(name, default):
        return {"pnpm_config__auth": setting}.get(name, default)

    # No .npmrc at all: the setting alone authenticates the scope's registry.
    asserts.equals(
        env,
        {_CASTLE_JS: _bearer("castle-token")},
        fetch_auth("", _CASTLE_JS, "@castleio/castle-js", getenv_setting),
    )
    asserts.equals(
        env,
        {_CASTLE_JS: _bearer("castle-token")},
        fetch_auth("", _CASTLE_JS, "@castleio/castle-js", getenv_lower),
    )

    # The package's scope entry outranks the .npmrc's credential on the same
    # registry; a package of another scope there still takes the .npmrc's.
    asserts.equals(
        env,
        {_ACME_WIDGET: _bearer("acme-scoped")},
        fetch_auth(_NPMRC, _ACME_WIDGET, "@acme/widget", getenv_setting),
    )
    acme_zod = "https://npm.acme.test/zod/-/zod-3.24.2.tgz"
    granted = fetch_auth(_NPMRC, acme_zod, "zod", getenv_setting)
    asserts.equals(env, "YWNtZTpodW50ZXIy", _password(granted, acme_zod))

    # "@" on one registry outranks the .npmrc's line for it, for a scoped
    # package too; a path it does not cover keeps the .npmrc's host-wide token.
    asserts.equals(
        env,
        {virtual_zod: _bearer("virtual-from-setting")},
        fetch_auth(_NPMRC, virtual_zod, "zod", getenv_setting),
    )
    types_react = _VIRTUAL + "/@types/react/-/react-19.0.0.tgz"
    asserts.equals(
        env,
        {types_react: _bearer("virtual-from-setting")},
        fetch_auth(_NPMRC, types_react, "@types/react", getenv_setting),
    )
    granted = fetch_auth(_NPMRC, _ELSEWHERE, "zod", getenv_setting)
    asserts.equals(env, "host-wide-token", _password(granted, _ELSEWHERE))

    # A token granted to one port stays off another.
    one_port = json.encode({"http://127.0.0.1:4873": {"@": {"authToken": "t"}}})

    def getenv_one_port(name, default):
        return {"PNPM_CONFIG__AUTH": one_port}.get(name, default)

    asserts.equals(env, {}, fetch_auth(
        "",
        "http://127.0.0.1:4874/@acme/widget/-/widget-1.2.3.tgz",
        "@acme/widget",
        getenv_one_port,
    ))

    return unittest.end(env)

fetch_auth_test = unittest.make(_fetch_auth_test)

def _patched_dependencies_test(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, {
        "@acme/diffs@1.3.1": "1111111111111111111111111111111111111111111111111111111111111111",
        "acme-editor@1.41.4": "2222222222222222222222222222222222222222222222222222222222222222",
    }, parse_patched_dependencies(_LOCKFILE))

    asserts.equals(env, {}, parse_patched_dependencies("lockfileVersion: '9.0'\n"))

    return unittest.end(env)

patched_dependencies_test = unittest.make(_patched_dependencies_test)

def _patch_file_names_test(ctx):
    env = unittest.begin(ctx)

    # pnpm's own naming, and the only bridge from a lockfile key to a label: a
    # `/` cannot be in a filename, so a scoped name keeps its leading '@' after
    # the replacement -- the one filename shape that cannot be a glob() result.
    asserts.equals(env, "@acme__diffs@1.3.1.patch", patch_file_name("@acme/diffs@1.3.1"))
    asserts.equals(env, "acme-editor@1.41.4.patch", patch_file_name("acme-editor@1.41.4"))

    for pkg_id in parse_patched_dependencies(_LOCKFILE):
        asserts.false(env, "/" in patch_file_name(pkg_id), pkg_id)

    return unittest.end(env)

patch_file_names_test = unittest.make(_patch_file_names_test)

# ─── Integrity ────────────────────────────────────────────────────────────────
#
# Every `packages:` entry in every lockfile in this repository carries a
# `sha512-` integrity, so this is the only place the shapes that cannot supply
# one are represented: a git or remote tarball pnpm could not hash, a `file:`
# dependency on a local directory, and a digest old enough to predate SRI.
_UNVERIFIABLE_LOCKFILE = """lockfileVersion: '9.0'

packages:

  hashed@1.0.0:
    resolution: {integrity: sha512-hashed==}

  tarball-only@2.0.0:
    resolution: {tarball: https://example.test/tarball-only-2.0.0.tgz}

  local-directory@3.0.0:
    resolution:
      directory: packages/local-directory
      type: directory

  ancient@4.0.0:
    resolution: {integrity: sha1-YWJj}

snapshots:

  hashed@1.0.0: {}

  tarball-only@2.0.0: {}

  local-directory@3.0.0: {}

  ancient@4.0.0: {}
"""

def _integrity_test(ctx):
    env = unittest.begin(ctx)
    packages = parse_pnpm_lock(_UNVERIFIABLE_LOCKFILE)["packages"]
    reported = verify_integrity(packages)

    asserts.equals(
        env,
        ["ancient@4.0.0", "local-directory@3.0.0", "tarball-only@2.0.0"],
        [entry.package_id for entry in reported],
    )

    # The keys travel with the entry so the error can echo the shape it found
    # rather than switch on the shapes we happen to have seen.
    by_id = {entry.package_id: entry for entry in reported}
    asserts.equals(env, ["tarball"], sorted(by_id["tarball-only@2.0.0"].resolution.keys()))
    asserts.equals(env, ["directory", "type"], sorted(by_id["local-directory@3.0.0"].resolution.keys()))

    # The fixtures the rest of this repository builds from, pinned clean: the
    # gate is unconditional, so one entry short of a digest fails every build.
    asserts.equals(env, [], verify_integrity(parse_pnpm_lock(_LOCKFILE)["packages"]))

    return unittest.end(env)

integrity_test = unittest.make(_integrity_test)

# ─── Cycle breaking ───────────────────────────────────────────────────────────
#
# A dropped edge and a kept one fail in opposite directions and neither shows up
# as a cycle: dropping one edge too many hands a package a dependency Bazel
# never builds, and keeping one too few is the cycle Bazel refuses to load.

def _edges(broken):
    return sorted(["{} -> {}".format(frm, to) for frm, to in broken])

def _surviving(graph, broken):
    dropped = {"{} -> {}".format(frm, to): True for frm, to in broken}
    return {
        node: [dep for dep in deps if "{} -> {}".format(node, dep) not in dropped]
        for node, deps in graph.items()
    }

def _assert_acyclic(env, graph, broken):
    asserts.equals(env, [], break_cycles(_surviving(graph, broken)), "still cyclic after subtracting the broken edges")

def _cycles_acyclic_graph_untouched_test(ctx):
    env = unittest.begin(ctx)

    # A dep that is not a node of its own -- an @npm alias label, a workspace
    # link -- is outside the graph and cannot be part of a cycle in it.
    graph = {"a": ["b", "c", "@npm//:external"], "b": ["c"], "c": [], "d": ["a"]}
    asserts.equals(env, [], break_cycles(graph))

    return unittest.end(env)

cycles_acyclic_graph_untouched_test = unittest.make(_cycles_acyclic_graph_untouched_test)

def _cycles_self_edge_broken_test(ctx):
    env = unittest.begin(ctx)

    # A package that depends on itself is its own one-member component, so an
    # SCC-size test never sees it, and the self-edge reaches Bazel as a cycle.
    graph = {"a": ["a"], "b": ["a"]}
    broken = break_cycles(graph)

    asserts.equals(env, ["a -> a"], _edges(broken))
    asserts.equals(env, {"a": [], "b": ["a"]}, _surviving(graph, broken))
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_self_edge_broken_test = unittest.make(_cycles_self_edge_broken_test)

def _cycles_two_node_cycle_broken_test(ctx):
    env = unittest.begin(ctx)

    graph = {"core": ["helper", "safe"], "helper": ["core"], "safe": []}
    broken = break_cycles(graph)

    asserts.equals(env, ["helper -> core"], _edges(broken))
    asserts.equals(env, {"core": ["helper", "safe"], "helper": [], "safe": []}, _surviving(graph, broken))
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_two_node_cycle_broken_test = unittest.make(_cycles_two_node_cycle_broken_test)

def _cycles_three_node_cycle_broken_test(ctx):
    env = unittest.begin(ctx)

    # One back edge is the whole cycle; the other two are the build order that
    # is left, and dropping them costs a real dependency for nothing.
    graph = {"a": ["b"], "b": ["c"], "c": ["a"]}
    broken = break_cycles(graph)

    asserts.equals(env, ["c -> a"], _edges(broken))
    asserts.equals(env, {"a": ["b"], "b": ["c"], "c": []}, _surviving(graph, broken))
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_three_node_cycle_broken_test = unittest.make(_cycles_three_node_cycle_broken_test)

def _cycles_edge_between_two_cycles_survives_test(ctx):
    env = unittest.begin(ctx)

    # b -> c joins two cycles and is in neither of them: the condensation is a
    # DAG, so no edge across it can ever close a cycle.
    graph = {"a": ["b"], "b": ["a", "c"], "c": ["d"], "d": ["c"]}
    broken = break_cycles(graph)

    asserts.equals(env, ["b -> a", "d -> c"], _edges(broken))
    asserts.equals(env, {"a": ["b"], "b": ["c"], "c": ["d"], "d": []}, _surviving(graph, broken))
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_edge_between_two_cycles_survives_test = unittest.make(_cycles_edge_between_two_cycles_survives_test)

def _cycles_overlapping_peer_cycles_test(ctx):
    env = unittest.begin(ctx)

    # Two circular peer references sharing a node: one SCC, and the two edges
    # that close them are the only ones that may go.
    core = "@babel/core@7.26.0"
    helpers = "@babel/helpers@7.26.0(@babel/core@7.26.0)"
    traverse = "@babel/traverse@7.26.0(@babel/core@7.26.0)"
    graph = {
        core: [helpers, traverse],
        helpers: [core],
        traverse: [helpers, core],
    }
    broken = break_cycles(graph)

    asserts.equals(env, ["{} -> {}".format(helpers, core), "{} -> {}".format(traverse, core)], _edges(broken))
    asserts.equals(env, {core: [helpers, traverse], helpers: [], traverse: [helpers]}, _surviving(graph, broken))
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_overlapping_peer_cycles_test = unittest.make(_cycles_overlapping_peer_cycles_test)

def _cycles_peer_variants_are_separate_cycles_test(ctx):
    env = unittest.begin(ctx)

    # One name, two peer sets, two independent cycles. Merging them by name --
    # or pooling every cycle node into one set -- would drop the app's edges.
    graph = {
        "app": ["react@18.3.1", "react@19.0.0"],
        "react@18.3.1": ["react-dom@18.3.1(react@18.3.1)"],
        "react-dom@18.3.1(react@18.3.1)": ["react@18.3.1"],
        "react@19.0.0": ["react-dom@19.0.0(react@19.0.0)"],
        "react-dom@19.0.0(react@19.0.0)": ["react@19.0.0"],
    }
    broken = break_cycles(graph)

    asserts.equals(env, [
        "react-dom@18.3.1(react@18.3.1) -> react@18.3.1",
        "react-dom@19.0.0(react@19.0.0) -> react@19.0.0",
    ], _edges(broken))
    asserts.equals(env, ["react@18.3.1", "react@19.0.0"], _surviving(graph, broken)["app"])
    _assert_acyclic(env, graph, broken)

    return unittest.end(env)

cycles_peer_variants_are_separate_cycles_test = unittest.make(_cycles_peer_variants_are_separate_cycles_test)

def parser_test_suite(name):
    failures = []
    for suffix, content in {
        "flow": "registries: {'@acme': 'https://npm.acme.test/'}\n",
        "multiline_flow": "registries:\n  {\n    '@acme': 'https://npm.acme.test/'\n  }\n",
        "alias": "private: &private {'@acme': 'https://npm.acme.test/'}\nregistries: *private\n",
    }.items():
        target = name + "_" + suffix

        # Wildcard builds must not analyze this deliberately failing fixture.
        _registry_parse(name = target, content = content, tags = ["manual"])
        failures.append(partial.make(_unsupported_registry_test, target_under_test = ":" + target))
    unittest.suite(
        name,
        catalogs_are_not_importers_test,
        alias_values_are_snapshot_ids_test,
        catalogs_and_overrides_are_not_packages_test,
        platform_constraints_are_read_test,
        overrides_arrive_pre_resolved_test,
        package_extensions_arrive_as_edges_test,
        patched_dependencies_test,
        patch_file_names_test,
        integrity_test,
        snapshots_keep_peer_variants_test,
        peer_tokens_separate_peer_sets_test,
        importers_resolve_per_importer_test,
        importer_links_are_importer_relative_test,
        inline_importer_test,
        npmrc_registries_test,
        npmrc_registry_picks_the_url_test,
        pnpm_workspace_registries_test,
        workspace_registries_test,
        fetch_auth_test,
        cycles_acyclic_graph_untouched_test,
        cycles_self_edge_broken_test,
        cycles_two_node_cycle_broken_test,
        cycles_three_node_cycle_broken_test,
        cycles_edge_between_two_cycles_survives_test,
        cycles_overlapping_peer_cycles_test,
        cycles_peer_variants_are_separate_cycles_test,
        file_tarball_snapshot_keeps_key_and_version_test,
        *failures
    )
