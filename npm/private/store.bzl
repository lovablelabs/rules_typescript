"""The virtual store: one tree of real files per lockfile snapshot, its
dependency links beside it. docs/rules/node-modules.md § The Store."""

load(
    "//ts/private:providers.bzl",
    "NpmHoistInfo",
    "NpmLinkInfo",
    "TsInfo",
    "canonical_runtime_file",
    "runtime_links",
    "runtime_mappings",
)
load(
    "//ts/private:toolchain.bzl",
    "TOOLS_TOOLCHAIN_TYPE",
    "get_tools_toolchain",
)

NpmStoreInfo = provider(
    doc = "One snapshot of the virtual store: its tree and the links beside " +
          "it.",
    fields = {
        "key": "string: the store directory's name, " +
               "`<name with / as +>@<version>[_<peer id>]`.",
        "tree": "File: the tree artifact of the snapshot's files, " +
                "`node_modules/.pnpm/<key>/node_modules/<name>`.",
        "links": "dict of string -> File: the dependency links beside the " +
                 "tree, by the name the snapshot imports each dependency by; " +
                 "a cut edge's among them.",
        "transitive": "depset of File: the tree, its links and every " +
                      "dependency store's transitive set, a cut edge's " +
                      "excepted.",
    },
)

MEMBER_VERSION = "0.0.0"

STORE_DIR = "node_modules/.pnpm"

HOIST_TARGET = STORE_DIR + "/node_modules"

def store_key(name, version, peer_id):
    """The store directory of one resolution: name, version and peer set."""
    key = "{}@{}".format(name.replace("/", "+"), version)
    return key if not peer_id else "{}_{}".format(key, peer_id)

def store_target(key, name):
    """The name of a store target, which is its tree's path in the package."""
    return "{}/{}/node_modules/{}".format(STORE_DIR, key, name)

def _store_parts(name):
    parts = name.split("/")
    shaped = (
        len(parts) >= 5 and parts[0] == "node_modules" and
        parts[1] == ".pnpm" and parts[3] == "node_modules"
    )
    if not shaped:
        fail(
            "npm_store: a store target is named after its tree, " +
            "node_modules/.pnpm/<key>/node_modules/<package>; got '{}'".format(
                name,
            ),
        )
    return struct(
        key = parts[2],
        package = "/".join(parts[4:]),
        links_dir = "/".join(parts[:4]),
    )

def _relative(from_dir, to):
    a = from_dir.split("/")
    b = to.split("/")
    shared = 0
    for _ in range(min(len(a), len(b))):
        if a[shared] != b[shared]:
            break
        shared += 1
    return "/".join([".."] * (len(a) - shared) + b[shared:])

def store_link(ctx, dir, name, store):
    """The declared symlink `<dir>/<name>`, its target the relative path to
    `store`'s tree."""
    link = ctx.actions.declare_symlink("{}/{}".format(dir, name))
    ctx.actions.symlink(
        output = link,
        target_path = _relative(link.dirname, store.tree.path),
    )
    return link

def _claim(ctx, name, linked, owner):
    if name in linked:
        fail("{}: '{}' linked twice, to {} and {}".format(
            ctx.label,
            name,
            linked[name],
            owner,
        ))
    linked[name] = owner

def _link(ctx, dir, name, dep, linked):
    _claim(ctx, name, linked, dep.label)
    return store_link(ctx, dir, name, dep[NpmStoreInfo])

def _link_by_name(ctx, dir, name, tree):
    """The declared symlink `<dir>/<name>` to `tree`, a path in this package,
    with no dependency behind it."""
    path = "{}/{}".format(dir, name)
    link = ctx.actions.declare_symlink(path)
    ctx.actions.symlink(
        output = link,
        target_path = _relative(path.rsplit("/", 1)[0], tree),
    )
    return link

def _dep_links(ctx, parts, cut = {}):
    links = {}
    linked = {}
    for dep, names in ctx.attr.deps.items():
        for name in names.split(" "):
            if name == parts.package:
                fail(
                    "{}: depends on {} under its own name '{}', ".format(
                        ctx.label,
                        dep.label,
                        name,
                    ) + "the path its tree holds; pnpm writes no such edge.",
                )
            links[name] = _link(ctx, parts.links_dir, name, dep, linked)
    for name, tree in cut.items():
        if name in links:
            fail("{}: '{}' is a dependency and a cut edge".format(
                ctx.label,
                name,
            ))
        links[name] = _link_by_name(ctx, parts.links_dir, name, tree)
    return links

def _stage(ctx, tree, files, dest):
    args = ctx.actions.args()
    args.use_param_file("@%s", use_always = True)
    args.set_param_file_format("multiline")
    args.add("-out=" + tree.path)
    args.add_all(
        files,
        map_each = lambda f: [f.path, dest(f)],
        allow_closure = True,
    )
    ctx.actions.run(
        executable = get_tools_toolchain(ctx).tsaction,
        arguments = ["stage", args],
        inputs = files,
        outputs = [tree],
        mnemonic = "NpmStore",
        progress_message = "Staging %{label}",
    )

def _store_info(ctx, parts, tree, links):
    return NpmStoreInfo(
        key = parts.key,
        tree = tree,
        links = links,
        transitive = depset(
            [tree] + links.values(),
            transitive = [d[NpmStoreInfo].transitive for d in ctx.attr.deps],
        ),
    )

def _npm_store_impl(ctx):
    parts = _store_parts(ctx.label.name)
    tree = ctx.actions.declare_directory(ctx.label.name)
    root = ctx.file.package_dir.dirname + "/"

    def dest(f):
        if not f.path.startswith(root):
            fail("{}: {} lies outside the package at {}".format(
                ctx.label,
                f.path,
                root,
            ))
        return f.path[len(root):]

    _stage(ctx, tree, ctx.files.files, dest)
    links = _dep_links(ctx, parts, ctx.attr.cut)
    info = _store_info(ctx, parts, tree, links)
    return [DefaultInfo(files = info.transitive), info]

_DEPS = attr.label_keyed_string_dict(
    providers = [NpmStoreInfo],
    doc = "Store target -> the space-separated names this snapshot imports " +
          "it by: one link beside the tree per name, an npm alias under the " +
          "alias name.",
)

npm_store = rule(
    implementation = _npm_store_impl,
    attrs = {
        "files": attr.label(
            mandatory = True,
            doc = "The snapshot repository's `:files` filegroup.",
        ),
        "package_dir": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The extracted package.json; its directory is the package " +
                  "root.",
        ),
        "deps": _DEPS,
        "cut": attr.string_dict(
            doc = "Link name -> the tree path, in this package, of a " +
                  "dependency the extension cut to break a cycle: a link " +
                  "beside the tree with no dependency behind it, as pnpm's " +
                  "store has it; the tree is in a closure through the edge " +
                  "that closes the cycle.",
        ),
    },
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    doc = """One store tree, named after its path: the snapshot's files copied
by `tsaction stage` into `node_modules/.pnpm/<key>/node_modules/<name>`, and
one declared symlink beside it per dependency, a cut edge's with no dependency
behind it. `npm_virtual_store` declares every one of a lockfile's; not for
hand use.""",
)

def _member_roots(ctx, member):
    parts = [member.label.workspace_root, ctx.attr.member_dir]
    src = "/".join([p for p in parts if p])
    return [ctx.bin_dir.path + "/" + src + "/", src + "/"]

def _npm_store_member_impl(ctx):
    parts = _store_parts(ctx.label.name)
    member = ctx.attr.member
    tree = ctx.actions.declare_directory(ctx.label.name)
    info = member[TsInfo]
    roots = _member_roots(ctx, member)
    label = str(ctx.label)
    member_dir = ctx.attr.member_dir

    def outside(f):
        return ("{}: npm_store_member cannot preserve the relative layout of " +
                "'{}' outside member '{}'. Did you mean to move the file " +
                "inside the member or import it through a separately " +
                "published package?").format(label, f.short_path, member_dir)

    def dest(f):
        if f == info.manifest:
            return "package.json"
        for root in roots:
            if f.path.startswith(root):
                return f.path[len(root):]
        fail(outside(f))

    data = info.data.to_list()

    # Every file outside the member is named at once; which came first in data is incidental.
    stray = {
        outside(f): True
        for f in data + [f for emitted in (info.declarations, info.js, info.js_maps, info.runtime_sources) for f in emitted.to_list()]
        if f != info.manifest and not [root for root in roots if f.path.startswith(root)]
    }
    if stray:
        fail("\n".join(stray.keys()))
    written = [f for f in data if dest(f) == "package.json"]
    if not info.manifest or not written:
        fail(("{}: {} stages no package.json at {}; the tree's manifest is " +
              "the one the member's compile writes as built, so list the " +
              "member's package.json in package_scopes, or srcs if imported as JSON.").format(
            ctx.label,
            member.label,
            ctx.attr.member_dir,
        ))
    files = [f for f in data if f not in written] + [info.manifest]
    for emitted in (info.declarations, info.js, info.js_maps, info.runtime_sources):
        files.extend(emitted.to_list())
    selected = {file: True for file in files}
    owners = info.owners.to_list()
    links = runtime_links(owners)
    if links:
        canonical = {file: canonical_runtime_file(file, links) for file in links if file in selected}

        # A scope imported as JSON also has a module identity that the store must preserve.
        modules = {
            canonical_runtime_file(file, links): True
            for file in depset(
                transitive = [info.transitive_js, info.transitive_runtime_sources] + [owner.declarations for owner in owners],
                order = "postorder",
            ).to_list()
        }
        live = {
            file: True
            for file in depset(
                transitive = [info.transitive_data, info.transitive_js, info.transitive_runtime_sources],
                order = "postorder",
            ).to_list()
        }
        for _owner, pairs in runtime_mappings(owners, live):
            for source, runtime in pairs:
                modules[canonical_runtime_file(source, links)] = True
                modules[canonical_runtime_file(runtime, links)] = True
        scope_aliases = {
            runtime: True
            for owner in owners
            for source, runtime in getattr(owner, "runtime_scopes", ())
            if source in links.get(runtime, {}) and canonical.get(runtime) == canonical_runtime_file(source, links) and canonical.get(runtime) not in modules
        }
        for owner in owners:
            for link, target in getattr(owner, "canonical_links", ()):
                if link in selected and link not in scope_aliases:
                    fail(("{}: member {} requires canonical dependency '{}' through '{}'; " +
                          "the npm store copies files and cannot preserve the dependency's package scope and importer. " +
                          "Use a separately published npm dependency or include its sources in this member.").format(
                        ctx.label,
                        member.label,
                        target.short_path,
                        link.short_path,
                    ))
    _stage(ctx, tree, files, dest)
    store = _store_info(ctx, parts, tree, _dep_links(ctx, parts))
    return [DefaultInfo(files = store.transitive), store]

npm_store_member = rule(
    implementation = _npm_store_member_impl,
    attrs = {
        "member": attr.label(
            mandatory = True,
            providers = [TsInfo],
            doc = "The target that compiles the member, as npm_hub finds it.",
        ),
        "member_dir": attr.string(
            mandatory = True,
            doc = "The member's directory from the workspace root.",
        ),
        "deps": _DEPS,
    },
    toolchains = [TOOLS_TOOLCHAIN_TYPE],
    doc = """A workspace member's store tree, `node_modules/.pnpm/<name with /
as +>@0.0.0/node_modules/<name>`: its `.js`, `.js.map`, `.d.ts` and data srcs
at their package-relative paths, the package.json as built in place of the
src; one declared symlink beside it per dependency the member's importer
declares. Declared by `npm_virtual_store`.""",
)

def _npm_store_hoist_impl(ctx):
    linked = {}
    links = {}
    members = {}
    for dir, entries, names in (
        (ctx.label.name, ctx.attr.private, ctx.attr.private_members),
        ("node_modules", ctx.attr.public, ctx.attr.public_members),
    ):
        for dep, aliases in entries.items():
            for name in aliases.split(" "):
                links[name] = NpmLinkInfo(
                    link = _link(ctx, dir, name, dep, linked),
                    store = dep[NpmStoreInfo],
                )
        for name in names:
            tree = store_target(store_key(name, MEMBER_VERSION, ""), name)
            _claim(ctx, name, linked, ctx.label.same_package_label(tree))
            members[name] = _link_by_name(ctx, dir, name, tree)
    return [
        DefaultInfo(files = depset(
            [entry.link for entry in links.values()] + members.values(),
            transitive = [entry.store.transitive for entry in links.values()],
        )),
        NpmHoistInfo(links = links, members = members),
    ]

_HOISTED = attr.label_keyed_string_dict(
    providers = [NpmStoreInfo],
    doc = "Store target -> the space-separated names linked at it.",
)

_MEMBERS = attr.string_list(
    doc = "The hoisted workspace members, by name: a link into each one's " +
          "tree with no dependency behind it.",
)

npm_store_hoist = rule(
    implementation = _npm_store_hoist_impl,
    attrs = {
        "private": _HOISTED,
        "public": _HOISTED,
        "private_members": _MEMBERS,
        "public_members": _MEMBERS,
    },
    doc = """The hidden hoist of one lockfile: a declared symlink into a store
tree per hoisted name, `private` ones under this target's name
(`node_modules/.pnpm/node_modules/<name>`), `public` ones at the root
importer's `node_modules/<name>`; a hoisted workspace member's
(`private_members`, `public_members`) by name alone, with no dependency on
the member's tree, since the root importer's `node_modules` names this target
and a member's compile walks up to it. `NpmHoistInfo` carries them for the
root importer's `hoist`. Declared by `npm_virtual_store`.""",
)

def _join(names):
    return " ".join(sorted(names))

def _platform_dicts(common, by_platform, platforms):
    if not by_platform:
        return {label: _join(names) for label, names in common.items()}
    branches = {}
    for platform in platforms:
        merged = {label: list(names) for label, names in common.items()}
        for label, names in by_platform.get(platform, {}).items():
            merged.setdefault(label, []).extend(names)
        branches[Label("//platforms:is_" + platform)] = {
            label: _join(names)
            for label, names in merged.items()
        }
    return select(branches, no_match_error = (
        "the npm store here is platform-specific and this target platform " +
        "is none of " + ", ".join(platforms)
    ))

def _add(common, by_platform, platforms, label, name):
    if platforms == None:
        common.setdefault(label, []).append(name)
        return
    for platform in platforms:
        by_platform.setdefault(platform, {}).setdefault(label, []).append(name)

def hoisted_on(graph, platform):
    """{alias: (kind, target)} the graph hoists on `platform`; a target is
    ("snapshot", index) or ("member", index) into the graph's lists."""
    out = {}
    for entry in graph["hoist"]:
        ref = entry.get("target", entry.get("targets", {}).get(platform))
        if ref == None:
            continue
        which, index = ref.items()[0]
        out[entry["alias"]] = (entry["kind"], (which, index))
    return out

def virtual_store(name, graph, members, files, package_dir):
    """Declares a lockfile's store in the calling package: every npm_store,
    npm_store_member and the npm_store_hoist, `manual` and public.

    Args:
        name: The store directory the call is named after, `node_modules/.pnpm`.
        graph: The hub's store graph, decoded.
        members: {member path: Label of its compiling target} for every
            member whose BUILD file declares it.
        files: function(repo) -> Label of the snapshot repository's `:files`,
            evaluated in the hub, which sees the repository by that name.
        package_dir: function(repo, name) -> Label of its package.json.
    """
    if name != STORE_DIR:
        fail(
            "npm_virtual_store: the store is pnpm's {}, ".format(STORE_DIR) +
            "which names the call; got '{}'".format(name),
        )
    here = (native.repo_name(), native.package_name())
    if here != (graph["repo"], graph["package"]):
        fail(
            "npm_virtual_store called from @@{}//{}: the store of {} ".format(
                here[0],
                here[1],
                graph["lockfile"],
            ) + "is declared in its own package, @@{}//{}".format(
                graph["repo"],
                graph["package"],
            ),
        )
    platforms = graph["platforms"]
    snapshots = graph["snapshots"]
    member_targets = {}
    for index, member in enumerate(graph["members"]):
        if member["path"] in members:
            member_targets[index] = store_target(member["key"], member["name"])

    def snapshot_target(index):
        snap = snapshots[index]
        return store_target(snap["key"], snap["name"])

    def edges(deps, links):
        common = {}
        by_platform = {}
        for index, alias in deps:
            dep = snapshots[index]
            label = ":" + snapshot_target(index)
            name = alias or dep["name"]
            _add(common, by_platform, dep.get("platforms"), label, name)
        for index, alias in links:
            if index in member_targets:
                label = ":" + member_targets[index]
                _add(common, by_platform, None, label, alias)
        return _platform_dicts(common, by_platform, platforms)

    for index, snap in enumerate(snapshots):
        npm_store(
            name = snapshot_target(index),
            files = files(snap["repo"]),
            package_dir = package_dir(snap["repo"], snap["name"]),
            deps = edges(snap["deps"], []),
            cut = {
                alias or snapshots[dep]["name"]: snapshot_target(dep)
                for dep, alias in snap["cut"]
            },
            tags = ["manual"],
            visibility = ["//visibility:public"],
        )

    for index, member in enumerate(graph["members"]):
        if index not in member_targets:
            continue
        npm_store_member(
            name = member_targets[index],
            member = members[member["path"]],
            member_dir = member["path"],
            deps = edges(member["deps"], member["links"]),
            tags = ["manual"],
            visibility = ["//visibility:public"],
        )

    seen = {}
    hoisted_members = {}
    for platform in platforms:
        for alias, (kind, ref) in hoisted_on(graph, platform).items():
            which, index = ref
            if which == "member":
                if index in member_targets:
                    hoisted_members[(kind, alias)] = True
                continue
            target = snapshot_target(index)
            seen.setdefault((kind, ":" + target, alias), []).append(platform)
    hoist = {"private": ({}, {}), "public": ({}, {})}
    for (kind, label, alias), on in seen.items():
        common, by_platform = hoist[kind]
        everywhere = len(on) == len(platforms)
        _add(common, by_platform, None if everywhere else on, label, alias)
    dicts = {
        kind: _platform_dicts(common, by_platform, platforms)
        for kind, (common, by_platform) in hoist.items()
    }
    npm_store_hoist(
        name = HOIST_TARGET,
        private = dicts["private"],
        public = dicts["public"],
        private_members = sorted([
            name
            for (kind, name) in hoisted_members
            if kind == "private"
        ]),
        public_members = sorted([
            name
            for (kind, name) in hoisted_members
            if kind == "public"
        ]),
        tags = ["manual"],
        visibility = ["//visibility:public"],
    )
