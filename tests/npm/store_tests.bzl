"""The virtual store at analysis: one NpmStore action per snapshot writing one
tree, the links beside it declared symlinks and nothing inside the tree, a cut
edge a link with no dependency behind it, a member's manifest as built, and
pnpm's hidden hoist over the fixture lockfile
(tests/npm/hoisted_dependencies.bzl, pnpm's own answer)."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("@npm//:defs.bzl", "store_graph")
load("//npm/private:hoist.bzl", "hoist_settings", "matches")
load("//npm/private:npm_translate_lock.bzl", "peer_suffix_dir_name")
load("//npm/private:store.bzl", "NpmStoreInfo", "hoisted_on")
load(":hoisted_dependencies.bzl", "HOISTED", "HOISTED_MEMBERS")

_FEATURES = "tests/npm/features/node_modules/.pnpm/"

def _actions(env, mnemonic):
    actions = analysistest.target_actions(env)
    return [a for a in actions if a.mnemonic == mnemonic]

def staged_inputs(staged):
    """The files an NpmStore action copies: its inputs without the tool and
    its params file."""
    if not staged:
        return []
    return [
        f
        for f in staged[0].inputs.to_list()
        if f.extension != "params" and "/ts/tools/tsaction/" not in f.path
    ]

def _outputs(env):
    out = []
    for action in analysistest.target_actions(env):
        out.extend(action.outputs.to_list())
    return out

def _store_tree_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[NpmStoreInfo]
    asserts.equals(env, ctx.attr.key, info.key, "the store key")

    staged = _actions(env, "NpmStore")
    asserts.equals(env, 1, len(staged), "one NpmStore action")
    if len(staged) == 1:
        outputs = staged[0].outputs.to_list()
        asserts.equals(env, [info.tree], outputs, "its one output is the tree")
    asserts.true(env, info.tree.is_directory, "the tree is a tree artifact")
    path = _FEATURES + ctx.attr.key + "/node_modules/" + ctx.attr.package
    asserts.true(
        env,
        info.tree.short_path.endswith(path),
        "the tree sits at its store path: " + info.tree.short_path,
    )

    inside = [
        f.path
        for f in _outputs(env)
        if f.path.startswith(info.tree.path + "/")
    ]
    asserts.equals(env, [], inside, "no output lies inside the tree")

    asserts.equals(
        env,
        sorted(ctx.attr.links),
        sorted(info.links.keys()),
        "the links beside the tree",
    )
    for name, link in info.links.items():
        asserts.true(env, link.is_symlink, name + " is a declared symlink")
        asserts.equals(
            env,
            info.tree.dirname,
            link.dirname,
            "{} sits in the store's node_modules beside the tree".format(name),
        )
    dep_trees = sorted([
        f.short_path[f.short_path.find(_FEATURES) + len(_FEATURES):]
        for f in info.transitive.to_list()
        if f.is_directory and f != info.tree
    ])
    asserts.equals(
        env,
        sorted(ctx.attr.dep_trees),
        dep_trees,
        "the transitive set holds the dependency trees",
    )
    return analysistest.end(env)

store_tree_test = analysistest.make(
    _store_tree_impl,
    attrs = {
        "key": attr.string(mandatory = True),
        "package": attr.string(mandatory = True),
        "links": attr.string_list(),
        "dep_trees": attr.string_list(),
    },
)

_ESLINT = "//tests/eslint:node_modules/.pnpm/eslint@10.14.0/node_modules/eslint"

_ESLINT_UTILS = "@eslint-community+eslint-utils@4.10.1_eslint_10_14_0_cb57019c"

# eslint -> @eslint-community/eslint-utils closes a cycle the extension cut.
def _cut_edge_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[NpmStoreInfo]
    name = "@eslint-community/eslint-utils"
    asserts.true(env, name in info.links, "the cut edge is a link by the tree")
    if name in info.links:
        asserts.true(env, info.links[name].is_symlink, "a declared symlink")
        asserts.equals(
            env,
            info.tree.dirname + "/" + name,
            info.links[name].path,
            "in the store's node_modules beside the tree",
        )
    asserts.equals(
        env,
        [],
        [
            f.short_path
            for f in info.transitive.to_list()
            if f.is_directory and _ESLINT_UTILS in f.short_path
        ],
        "no dependency behind the link: eslint-utils' tree is not in " +
        "eslint's transitive set",
    )
    return analysistest.end(env)

cut_edge_test = analysistest.make(_cut_edge_impl)

def _member_store_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[NpmStoreInfo]
    asserts.equals(env, "shared@0.0.0", info.key, "a member's key")

    staged = _actions(env, "NpmStore")
    asserts.equals(env, 1, len(staged), "one NpmStore action")
    root = "/packages/shared/"
    copied = sorted([
        f.path[f.path.find(root) + len(root):] if root in f.path else f.basename
        for f in staged_inputs(staged)
    ])
    asserts.equals(
        env,
        [
            "shared.package.json",
            "src/banner.json",
            "src/index.d.ts",
            "src/index.js",
            "src/index.js.map",
            "src/wire/index.d.ts",
            "src/wire/index.js",
            "src/wire/index.js.map",
            "src/wire/package.json",
        ],
        copied,
        "the tree holds the member's .js and .d.ts at the paths the manifest " +
        "names and its data srcs at their package-relative paths, the " +
        "manifest as built in place of the package.json src",
    )
    asserts.equals(env, ["zod"], info.links.keys(), "the importer's dependency")
    return analysistest.end(env)

member_store_test = analysistest.make(_member_store_impl)

def _hoist_impl(ctx):
    env = unittest.begin(ctx)
    by_index = {}
    members = []
    hoist = hoisted_on(store_graph, "linux_amd64")
    for alias, (kind, (which, index)) in hoist.items():
        if which == "member":
            members.append(alias)
            continue
        sid = store_graph["snapshots"][index]["id"]
        by_index.setdefault(sid, {})[alias] = kind
    asserts.equals(env, HOISTED, by_index, "the packages pnpm's hoist links")
    asserts.equals(env, HOISTED_MEMBERS, sorted(members), "the members")
    return unittest.end(env)

hoist_test = unittest.make(_hoist_impl)

_SETTINGS_CASES = [
    struct(
        npmrc = None,
        private = ["*"],
        public = [],
        workspace_packages = True,
    ),
    struct(
        npmrc = "hoist=false\n",
        private = [],
        public = [],
        workspace_packages = True,
    ),
    struct(
        npmrc = "hoist-pattern[]=*eslint*\nhoist-pattern[]=!eslint-x\n" +
                "public-hoist-pattern[]=*prettier*\n",
        private = ["*eslint*", "!eslint-x"],
        public = ["*prettier*"],
        workspace_packages = True,
    ),
    struct(
        npmrc = "public-hoist-pattern=\nhoist-workspace-packages=false\n",
        private = ["*"],
        public = [],
        workspace_packages = False,
    ),
]

_WORKSPACE_SETTINGS_CASES = [
    struct(
        failure = "workspace public hoist is lost among unrelated settings",
        npmrc = None,
        workspace = "packages:\n  - web\npublicHoistPattern:\n  - '@bufbuild/protobuf' # generated source runtime\ncatalog:\n  hoist: false\n",
        private = ["*"],
        public = ["@bufbuild/protobuf"],
        workspace_packages = True,
    ),
    struct(
        failure = "npmrc patterns survive an overriding YAML list",
        npmrc = "public-hoist-pattern[]=old\npublic-hoist-pattern[]=other\nhoist-pattern[]=old\nhoist-workspace-packages=false\n",
        workspace = 'publicHoistPattern: ["@bufbuild/protobuf", "!excluded"]\nhoistPattern:\n- "*types*"\nhoistWorkspacePackages: true\n',
        private = ["*types*"],
        public = ["@bufbuild/protobuf", "!excluded"],
        workspace_packages = True,
    ),
    struct(
        failure = "explicit empty YAML patterns fall back to npmrc",
        npmrc = "public-hoist-pattern[]=old\nhoist-pattern[]=old\n",
        workspace = "publicHoistPattern: []\nhoistPattern: []\n",
        private = [],
        public = [],
        workspace_packages = True,
    ),
    struct(
        failure = "disabled private hoisting removes requested public links",
        npmrc = "hoist=true\n",
        workspace = 'hoist: false\nhoistPattern: ["*"]\npublicHoistPattern: ["public"]\nhoistWorkspacePackages: false\n',
        private = [],
        public = ["public"],
        workspace_packages = False,
    ),
    struct(
        failure = "absent YAML setting erases npmrc patterns",
        npmrc = "public-hoist-pattern[]=retained\nhoist=false\n",
        workspace = "packages:\n  - web\nhoist: true\n",
        private = ["*"],
        public = ["retained"],
        workspace_packages = True,
    ),
]

_MATCH_CASES = [
    (["*"], "@scope/name", True),
    (["*eslint*"], "@eslint/js", True),
    (["*eslint*"], "prettier", False),
    (["eslint"], "eslint-plugin", False),
    (["*eslint*", "!eslint-plugin-x"], "eslint-plugin-x", False),
    (["!eslint"], "prettier", True),
    (["!eslint"], "eslint", False),
    ([], "anything", False),
]

def _settings_impl(ctx):
    env = unittest.begin(ctx)
    for case in _SETTINGS_CASES:
        got = hoist_settings(case.npmrc)
        asserts.equals(env, case.private, got.private, str(case.npmrc))
        asserts.equals(env, case.public, got.public, str(case.npmrc))
        asserts.equals(
            env,
            case.workspace_packages,
            got.workspace_packages,
            str(case.npmrc),
        )
    for case in _WORKSPACE_SETTINGS_CASES:
        got = hoist_settings(case.npmrc, case.workspace)
        asserts.equals(env, case.private, got.private, case.failure)
        asserts.equals(env, case.public, got.public, case.failure)
        asserts.equals(env, case.workspace_packages, got.workspace_packages, case.failure)
    for patterns, name, want in _MATCH_CASES:
        asserts.equals(
            env,
            want,
            matches(patterns, name),
            "{} against {}".format(patterns, name),
        )
    return unittest.end(env)

settings_test = unittest.make(_settings_impl)

def _peer_key(peer_suffix):
    return "ansi-styles@6.2.3_" + peer_suffix_dir_name(peer_suffix)

_PEER_KEYS = [_peer_key("(ansi-regex@5.0.1)"), _peer_key("(ansi-regex@6.2.2)")]

def peer_variant_stores():
    """The features lockfile's two ansi-styles stores, one per peer set."""
    return [
        "//tests/npm/features:node_modules/.pnpm/{}/node_modules/".format(key) +
        "ansi-styles"
        for key in _PEER_KEYS
    ]

def store_test_suite(name):
    """The store tests over the features lockfile's peer variants, the
    fixture lockfile's `shared` member and hidden hoist, and the eslint
    lockfile's cut edge."""
    a, b = peer_variant_stores()
    store_tree_test(
        name = name + "_peer_a",
        key = _PEER_KEYS[0],
        package = "ansi-styles",
        links = ["ansi-regex"],
        dep_trees = ["ansi-regex@5.0.1/node_modules/ansi-regex"],
        target_under_test = a,
    )
    store_tree_test(
        name = name + "_peer_b",
        key = _PEER_KEYS[1],
        package = "ansi-styles",
        links = ["ansi-regex"],
        dep_trees = ["ansi-regex@6.2.2/node_modules/ansi-regex"],
        target_under_test = b,
    )
    member_store_test(
        name = name + "_member",
        target_under_test = "//tests/npm:node_modules/.pnpm/shared@0.0.0/" +
                            "node_modules/shared",
    )
    cut_edge_test(
        name = name + "_cut_edge",
        target_under_test = _ESLINT,
    )
    unittest.suite(name + "_hoist", hoist_test, settings_test)
    native.test_suite(
        name = name,
        tests = [
            ":" + name + "_peer_a",
            ":" + name + "_peer_b",
            ":" + name + "_member",
            ":" + name + "_cut_edge",
            ":" + name + "_hoist",
        ],
    )
