"""Declares one repository per resolved npm package, plus an alias hub.

A single repository for the whole lockfile has to download everything before it
can generate any target: it reads `bin` and `exports` out of each extracted
`package.json`. On a 2731-package lockfile that is roughly five minutes and five
gigabytes, paid in full by a target whose only npm dependency is vitest, in one
serial Starlark loop with no resume.

Here the extension does the whole-graph analysis -- which it can do from the
lockfile text alone, with no network -- and then declares one `npm_import` per
resolved package. Each one reads its own `package.json` and writes its own BUILD
file, so Bazel fetches repositories on demand.

The unit is the SNAPSHOT, not the package
-----------------------------------------
pnpm resolves a package once per distinct peer set and writes each outcome as its
own `snapshots:` key: `@storybook/react@8.6.14(react@18.3.1)` and
`@storybook/react@8.6.14(react@19.0.0)` are one tarball and two different
dependency graphs. Keying anything by `name@version` merges them, and the merge
is silent -- every version in the merged result is a real version, so the only
symptom is an importer built against the other variant's transitive closure.

So the graph is keyed by snapshot id throughout, and a snapshot's repository name
carries its peer set. A peer-free snapshot keeps the plain `<name>__<version>`
repository name it has always had.

The same peer token travels on to `NpmPackageInfo.peer_id`: a `node_modules` tree
has to give two peer resolutions of one version two directories for the same
reason two versions need two, and a provider carrying only name and version
cannot tell it there are two.

Resolution is per importer
--------------------------
`importers:` records, for each workspace member, the snapshot each declared
dependency resolved to -- and two members legitimately resolve one name to
different majors. A single flat hub cannot express that, so each importer gets
its own package inside the hub: `@npm//path/to/member:react` is what that member
declared, while `@npm//:react` is the whole-lockfile namespace (the root
importer's resolution where it has one, the highest version otherwise).

The analysis stays here rather than moving into the packages because all of it is
a whole-graph decision that one package cannot make about itself:

  label assignment      `@npm//:react` means the highest version present, which
                        is only knowable across the whole lockfile
  @types pairing        matching react@19 to @types/react@19 requires seeing both
  cycle breaking        npm has genuine circular peer dependencies and a Bazel
                        target graph may not contain a cycle
  alias naming          an npm alias is a name no package claims as its own, so
                        only the graph knows the alias exists
  patch routing         `patchedDependencies` is keyed by name@version at the
                        top of the lockfile, nowhere near the package it patches
  platform partition    which platforms a tarball is for decides which select()
                        branch may reference it
"""

load("//npm/private:hoist.bzl", "hoist_settings", "hoisted")
load(
    "//npm/private:npm_import.bzl",
    "npm_hub",
    "npm_import",
)
load(
    "//npm/private:npm_translate_lock.bzl",
    "local_tarball_path",
    "npm_tarball_url",
    "package_name_to_label",
    "parse_importers",
    "parse_patched_dependencies",
    "parse_pnpm_lock",
    "peer_suffix_dir_name",
    "pkg_matches_platform",
    "semver_gt",
    "semver_parts",
    "snapshot_dir_name",
    "verify_integrity",
    "versioned_label_name",
    "workspace_registries",
)
load("//npm/private:store.bzl", "MEMBER_VERSION", "store_key", "store_target")
load(
    "//platforms:platforms.bzl",
    "PLATFORMS",
)

_ALL_PLATFORMS = sorted(PLATFORMS)

def _repo_name(prefix, snapshot):
    return "{}__{}".format(prefix, snapshot_dir_name(snapshot))

def _alias_target_name(alias_package):
    """Target name inside the aliased package's own repository.

    The `alias_` prefix keeps it clear of `pkg` and `bin`, and the shared
    name-to-label rule keeps a scoped alias (`@my/thing`) out of subpackage
    territory.
    """
    return "alias_{}".format(package_name_to_label(alias_package))

_HEX = "0123456789abcdef"

def _is_sha256_hex(value):
    """Whether the lockfile's recorded digest is one we can check the file against.

    pnpm 9 and later record the plain sha256 of the patch bytes. Older lockfiles
    record something else, and a digest we cannot interpret is not a digest we
    may reject a file over.
    """
    if len(value) != 64:
        return False
    for ch in value.elems():
        if ch not in _HEX:
            return False
    return True

def patch_file_name(package_id):
    """The patch filename pnpm writes for a `patchedDependencies` key.

    `pnpm patch-commit` cannot put a `/` in a filename, so it writes the package
    name with `/` replaced by `__`. A scoped package therefore produces a name
    that still starts with `@`, and that leading `@` is load-bearing: a target
    name beginning with it cannot come out of `glob()`.
    """
    return "{}.patch".format(package_id.replace("/", "__"))

def _patch_file_sha256(module_ctx, label):
    """sha256 of the patch file's bytes, or "" when the file cannot be read.

    Starlark has no hashing builtin, so the file is copied through the
    downloader purely for the digest it reports. Deliberately WITHOUT download's
    own sha256 argument: a checksummed request is served from the
    content-addressed repository cache without the file being read at all, so an
    edited patch would be replaced by the locked bytes on a warm cache.
    """
    module_ctx.watch(label)
    path = module_ctx.path(label)
    if not path.exists:
        return ""
    result = module_ctx.download(
        url = "file://" + str(path),
        output = "_patch_digest/{}".format(path.basename),
        allow_fail = True,
    )
    return result.sha256 if result.success else ""

def _patch_by_package(module_ctx, lock_content, patch_labels):
    """Pairs each patchedDependencies entry with the patch file that satisfies it.

    Matching is by filename, which is pnpm's own: `pnpm patch-commit` writes
    `patches/<name with / replaced by __>@<version>.patch`, and the lockfile key
    is `<name>@<version>`. The alternative -- reading `path:` out of
    pnpm-workspace.yaml -- cannot be done at all: turning `patches/foo.patch`
    into a label requires knowing where the consumer's Bazel package boundaries
    fall, which the extension has no way to discover.

    A filename match is not on its own evidence that the patch will be applied,
    so each paired label is resolved to a file here and checked against the
    digest pnpm recorded for it. Reading it in the extension is what makes the
    check unconditional: the same digest is checked again by the fetch that
    applies the patch, but that fetch only happens once something pulls the
    package into a build.

    Every direction is an error, because each means the build would not be
    building what pnpm installs:
      a declared patch with no label     the package would install unpatched
      a label naming no readable file    same, and nothing else would say so
      a file whose digest disagrees      pnpm never saw the patch being applied
      a patch file no entry declares     the patch is stale, or misnamed

    Returns:
        dict: {package_id: struct(label = Label, sha256 = str)}
    """
    patched = parse_patched_dependencies(lock_content)
    unclaimed = {}
    for label in patch_labels:
        unclaimed[label.name.split("/")[-1]] = label

    result = {}
    missing = []
    unreadable = []
    any_scoped = False
    mismatched = []
    for pkg_id, digest in patched.items():
        filename = patch_file_name(pkg_id)
        if filename not in unclaimed:
            missing.append("  {} -> expected a patch file named {}".format(pkg_id, filename))
            continue
        label = unclaimed.pop(filename)
        found = _patch_file_sha256(module_ctx, label)
        if not found:
            unreadable.append("  {}".format(label))
            any_scoped = any_scoped or filename.startswith("@")
        elif _is_sha256_hex(digest) and found != digest:
            mismatched.append("  {}\n    lockfile: {}\n    file:     {}".format(label, digest, found))
        else:
            result[pkg_id] = struct(
                label = label,
                sha256 = digest if _is_sha256_hex(digest) else "",
            )

    if unreadable:
        fail(
            "npm: patch labels passed to npm.translate_lock(patches = [...]) that resolve " +
            "to no readable file:\n" + "\n".join(unreadable) +
            "\nA label the extension cannot read is a patch nothing applies, and the " +
            "package then installs as published -- the lockfile says it must not.\n" +
            ("A patch whose name starts with '@' cannot be exported by " +
             "exports_files(glob([\"*.patch\"])): glob prefixes ':' onto such a result and " +
             "exports_files rejects that as a target name, which fails the whole package. " +
             "List those files literally.\n" if any_scoped else ""),
        )
    if mismatched:
        fail(
            "npm: patch files whose sha256 disagrees with the one pnpm-lock.yaml records " +
            "in patchedDependencies:\n" + "\n".join(mismatched) +
            "\npnpm writes that digest when it writes the patch, so the two disagreeing " +
            "means the patch changed without `pnpm install` being re-run. Bazel would " +
            "apply a patch pnpm never saw.",
        )
    if missing:
        fail(
            "npm: pnpm-lock.yaml declares patchedDependencies with no patch file passed " +
            "to npm.translate_lock(patches = [...]):\n" + "\n".join(missing) +
            "\nWithout the patch these packages would install as published, which is " +
            "exactly what the lockfile says they must not be.",
        )
    if unclaimed:
        fail(
            "npm: patch files passed to npm.translate_lock(patches = [...]) that no " +
            "patchedDependencies entry claims: {}\n".format(", ".join(sorted(unclaimed.keys()))) +
            "Either the lockfile is stale (re-run pnpm install) or the file is not named " +
            "after the package it patches (<name with / as __>@<version>.patch).",
        )
    return result

def _member_entry(name, path):
    """The hub's record of one workspace member.

    The member's PATH, not the label of a target inside it: which directory of a
    member holds its target is a Gazelle decision, read from BUILD files that do
    not exist yet when a module extension runs. npm_hub resolves it at fetch
    time instead, so a BUILD file appearing refetches one cheap repository
    rather than re-evaluating this extension -- whose result is what
    MODULE.bazel.lock records.

    The npm name travels beside the path because the hub target has to declare
    it: an alias would lose it, and the label name cannot carry it back
    ('types_react' is the label of both '@types/react' and 'types_react').
    """
    return "{name}|{path}".format(name = name, path = path)

def _member_manifest(module_ctx, workspace_root, path):
    """The member's named package.json as text and decoded, or None."""
    manifest = workspace_root.get_child(*(path.split("/") + ["package.json"]))
    module_ctx.watch(manifest)
    if not manifest.exists:
        return None
    text = module_ctx.read(manifest)
    decoded = json.decode(text)
    if type(decoded) != "dict":
        return None
    name = decoded.get("name")
    if type(name) != "string" or not name:
        return None
    return struct(text = text, decoded = decoded)

def platforms_of_package(pkg):
    """The PLATFORMS keys a published tarball is built for.

    Empty means the package targets a platform this ruleset has no vocabulary
    for (`os: [aix]`, `libc: [musl]`), and nothing may reference it.

    Args:
        pkg: A `packages:` entry from parse_pnpm_lock.

    Returns:
        A sorted list of PLATFORMS keys.
    """
    return [
        key
        for key in _ALL_PLATFORMS
        if pkg_matches_platform(
            pkg,
            PLATFORMS[key].npm_os,
            PLATFORMS[key].npm_cpu,
            PLATFORMS[key].npm_libc,
        )
    ]

def _dedup(items):
    return sorted({item: True for item in items}.keys())

def break_cycles(graph):
    """Finds the dependency edges that have to be dropped to leave `graph` acyclic.

    Every edge returned closes a cycle in a depth-first walk, which is what makes
    the result safe to subtract: an edge whose head is still on the walk's own
    path has a path back to itself, so it lies inside one strongly connected
    component. An edge between two distinct components can never qualify, and a
    self-edge always does.

    Args:
        graph: dict of node -> list of nodes it depends on. Not mutated. A dep
            that is not itself a key is outside the graph and never dropped.

    Returns:
        A list of (from_node, to_node) tuples.
    """
    nodes = sorted(graph)
    deps_of = {node: _dedup([dep for dep in graph[node] if dep in graph]) for node in nodes}

    budget = len(nodes) + 1
    for node in nodes:
        budget += 2 * len(deps_of[node]) + 2

    visited = {}
    on_path = {}
    cursor = {}
    broken = []
    for root in nodes:
        if root in visited:
            continue
        visited[root] = True
        on_path[root] = True
        cursor[root] = 0
        stack = [root]
        for _ in range(budget):
            if not stack:
                break
            node = stack[-1]
            deps = deps_of[node]
            if cursor[node] == len(deps):
                stack.pop()
                on_path.pop(node)
                continue
            dep = deps[cursor[node]]
            cursor[node] += 1
            if dep in on_path:
                broken.append((node, dep))
            elif dep not in visited:
                visited[dep] = True
                on_path[dep] = True
                cursor[dep] = 0
                stack.append(dep)
        if stack:
            fail("npm: cycle breaking ran out of steps walking '{}'; this is a bug in rules_typescript.".format(root))

    return broken

def _split_by_platform(labels_with_platforms):
    """Partitions dep labels into unconditional and per-platform lists.

    Args:
        labels_with_platforms: list of (label, [platform_key, ...]) pairs.

    Returns:
        (list of unconditional labels, {platform_key: [labels]}).
    """
    common = []
    per_platform = {}
    for (label, plats) in labels_with_platforms:
        if len(plats) == len(_ALL_PLATFORMS):
            common.append(label)
            continue
        for plat in plats:
            per_platform.setdefault(plat, []).append(label)
    return (_dedup(common), {plat: _dedup(labels) for plat, labels in per_platform.items()})

def _semver_of(snap):
    return semver_parts(snap.get("semver") or snap["version"])

def _pick_primary(sids, snapshots, preferred):
    """The snapshot a bare (or version-only) hub label should mean.

    `preferred` is the root importer's own resolution, which is the only answer
    the lockfile actually gives for that name; the highest-version fallback is a
    guess, and exists because the flat hub also has to name packages no importer
    declares.
    """
    for sid in sids:
        if sid in preferred:
            return sid
    best = None
    for sid in sorted(sids):
        if best == None:
            best = sid
            continue
        if semver_gt(_semver_of(snapshots[sid]), _semver_of(snapshots[best])):
            best = sid
    return best

def _check_integrity(packages, pnpm_lock):
    """Fails unless every `packages:` entry can be checked against its bytes.

    Whole-lockfile and at extension evaluation, for the reason _patch_by_package
    is: an entry no target depends on is checked too, so which package the error
    names does not depend on which target someone happened to build. Deciding it
    inside the fetch instead would make `bazel build //a` pass and
    `bazel build //b` fail on one lockfile.
    """
    unverifiable = verify_integrity(packages)
    if not unverifiable:
        return
    lines = [
        "  {} -> resolution keys: {}".format(
            entry.package_id,
            ", ".join(sorted(entry.resolution.keys())) if entry.resolution else "(none)",
        )
        for entry in unverifiable
    ]
    fail(
        "npm: entries in {} whose `resolution:` carries no usable integrity:\n".format(pnpm_lock) +
        "\n".join(lines) +
        "\nBazel would fetch these bytes with nothing to check them against, so a " +
        "registry that answered with something else would be indistinguishable from " +
        "one that answered correctly.\n" +
        "A git or local-directory dependency has no published tarball to " +
        "verify: depend on it as a workspace member (a `link:` entry, which becomes a " +
        "target in your own repository) or vendor its files. An entry that names a " +
        "`tarball:` but no integrity needs `pnpm install` re-run against a registry " +
        "that publishes one. A pre-SRI digest (sha1-) is not accepted: re-resolve the " +
        "lockfile with a current pnpm.",
    )

def _beside(module_ctx, pnpm_lock, name):
    """The text of the file `name` in the lockfile's directory, "" without one;
    the path is watched either way, so its appearance re-runs the extension."""
    path = module_ctx.path(pnpm_lock).dirname.get_child(name)
    if path.exists:
        return module_ctx.read(path)
    module_ctx.watch(path)
    return ""

def lockfile_registries(module_ctx, pnpm_lock, npmrc):
    return workspace_registries(
        module_ctx.read(npmrc) if npmrc else "",
        _beside(module_ctx, pnpm_lock, "pnpm-workspace.yaml"),
    )

def _store_label(pnpm_lock, key, name):
    return "@@{}//{}:{}".format(
        pnpm_lock.repo_name,
        pnpm_lock.package,
        store_target(key, name),
    )

def _local_tarball_label(pnpm_lock, path):
    """The label of a lockfile-relative `file:` tarball, or None when there is none."""
    if not path:
        return None
    if path.startswith("/") or ".." in path.split("/"):
        fail("npm: the `file:` tarball '{}' in {} must sit at or below the lockfile's directory".format(path, pnpm_lock))
    return "@@{}//{}:{}".format(pnpm_lock.repo_name, pnpm_lock.package, path)

def _snapshot_key(snap):
    peer_id = peer_suffix_dir_name(snap["peer_suffix"])
    return store_key(snap["name"], snap["version"], peer_id)

def _hoist_entries(lock, settings, index, member_index):
    """The graph's `hoist` list: per hoisted name its kind and target, one
    target when every platform hoists the same one, else one per platform."""

    def ref(target):
        which, key = target
        if which == "snapshot":
            return {"snapshot": index[key]}
        return {"member": member_index[key]}

    per_platform = {p: hoisted(lock, settings, p) for p in _ALL_PLATFORMS}
    aliases = {}
    for found in per_platform.values():
        aliases.update({alias: True for alias in found})
    entries = []
    for alias in sorted(aliases):
        on = {p: per_platform[p].get(alias) for p in _ALL_PLATFORMS}
        first = [t for t in on.values() if t != None][0]
        entry = {"alias": alias, "kind": first[0]}
        if all([on[p] == first for p in _ALL_PLATFORMS]):
            entry["target"] = ref(first[1])
        else:
            entry["targets"] = {
                p: ref(t[1])
                for p, t in on.items()
                if t != None
            }
        entries.append(entry)
    return entries

def _store_graph(pnpm_lock, graph, importers, settings):
    """The lockfile's store as data: what `npm_virtual_store` declares.

    Snapshots and members carry their edges as indices into the two lists;
    an edge the cycle breaker cut is a snapshot's `cut`, a link beside its
    tree with no dependency behind it, as it is absent from its repository's
    deps. The hoist is pnpm's over the whole graph, cycles kept.

    Args:
        pnpm_lock: The lockfile's label.
        graph: struct(live, platforms_of, repo_of, deps_by_sid, dropped,
            members): the extension's view of the lockfile, `members` as
            {member path: npm name} for the members with a manifest.
        importers: parse_importers() of the lockfile.
        settings: Effective hoist settings of the lockfile package.
    """
    live = graph.live
    sids = sorted(live)
    index = {sid: i for i, sid in enumerate(sids)}
    paths = sorted(graph.members)
    member_index = {path: i for i, path in enumerate(paths)}

    def edge(dep_sid, imported_as):
        return [index[dep_sid], imported_as]

    snapshots = []
    for sid in sids:
        snap = live[sid]
        entry = {
            "id": sid,
            "name": snap["name"],
            "key": _snapshot_key(snap),
            "repo": graph.repo_of[sid],
            "deps": [
                edge(dep_sid, imported_as)
                for (dep_sid, imported_as) in graph.deps_by_sid[sid]
                if not graph.dropped.get(sid, {}).get(dep_sid)
            ],
            "cut": [
                edge(dep_sid, imported_as)
                for (dep_sid, imported_as) in graph.deps_by_sid[sid]
                if graph.dropped.get(sid, {}).get(dep_sid)
            ],
        }
        if len(graph.platforms_of[sid]) != len(_ALL_PLATFORMS):
            entry["platforms"] = graph.platforms_of[sid]
        snapshots.append(entry)

    members = []
    for path in paths:
        importer = importers["importers"].get(path, {"deps": {}, "links": {}})
        name = graph.members[path]
        deps = []
        for dep_name, dep_sid in importer["deps"].items():
            if dep_sid not in live:
                continue
            aliased = live[dep_sid]["name"] != dep_name
            deps.append(edge(dep_sid, dep_name if aliased else ""))
        members.append({
            "name": name,
            "path": path,
            "key": store_key(name, MEMBER_VERSION, ""),
            "deps": deps,
            "links": [
                [member_index[linked], alias]
                for alias, linked in importer["links"].items()
                if linked in member_index
            ],
        })

    lock = struct(
        snapshots = {
            sid: struct(
                children = (
                    live[sid]["dependencies"].items() +
                    live[sid]["optionalDependencies"].items()
                ),
                platforms = graph.platforms_of[sid],
            )
            for sid in sids
        },
        importers = [
            (path, entry["deps"].items(), entry["links"].items())
            for path, entry in importers["importers"].items()
        ],
        members = [(graph.members[path], path) for path in paths],
    )
    return {
        "lockfile": str(pnpm_lock),
        "repo": pnpm_lock.repo_name,
        "package": pnpm_lock.package,
        "platforms": _ALL_PLATFORMS,
        "snapshots": snapshots,
        "members": members,
        "hoist": _hoist_entries(lock, settings, index, member_index),
    }

def declare_lazy_npm_repos(module_ctx, hub_name, pnpm_lock, patch_labels, npmrc):
    """Declares one npm_import per resolved package plus one npm_hub of aliases.

    Args:
        module_ctx:   The module extension context.
        hub_name:     Name of the alias hub repository, conventionally "npm".
        pnpm_lock:    Label of the pnpm-lock.yaml to read.
        patch_labels: Labels of the pnpm patch files named by patchedDependencies.
        npmrc:        Label of the workspace .npmrc, or None. Its registry lines
                      join pnpm-workspace.yaml's here; its credentials are
                      npm_import's to read at fetch time, because an extension's
                      output is recorded in MODULE.bazel.lock.
    """
    lock_content = module_ctx.read(pnpm_lock)

    # A member's package.json is found by path from the repository root, not by
    # label: a member need not be a Bazel package, so a label into one usually
    # fails to load. The root is the lockfile's directory with the lockfile's own
    # Bazel package climbed back out of, which is the directory `link:` paths and
    # importer keys are written against -- the same one link_target_label names as
    # `@@//<path>`. The two are one directory for every lockfile that sits at the
    # workspace root, and a lockfile that does not would otherwise have its
    # members looked for beside it and named somewhere else.
    workspace_root = module_ctx.path(pnpm_lock).dirname
    for _ in (pnpm_lock.package.split("/") if pnpm_lock.package else []):
        workspace_root = workspace_root.dirname
    registries = lockfile_registries(module_ctx, pnpm_lock, npmrc)
    parsed = parse_pnpm_lock(lock_content)
    packages = parsed["packages"]
    _check_integrity(packages, pnpm_lock)
    importers = parse_importers(lock_content)
    patches = _patch_by_package(module_ctx, lock_content, patch_labels)
    settings = hoist_settings(
        _beside(module_ctx, pnpm_lock, ".npmrc"),
        _beside(module_ctx, pnpm_lock, "pnpm-workspace.yaml"),
    )

    # A snapshot needs its `packages:` entry for the bytes to download, and needs
    # to be buildable on some platform we can name. Platform filtering is a
    # partition, not a drop: every platform's packages are declared and the
    # choice becomes a select(), so the extension result does not depend on the
    # machine that evaluated it.
    live = {}
    platforms_of = {}
    for sid, snap in parsed["snapshots"].items():
        pkg = packages.get(snap["package_id"])
        if pkg == None:
            continue
        plats = platforms_of_package(pkg)
        if not plats:
            continue
        live[sid] = snap
        platforms_of[sid] = plats

    # Resolve every dependency edge to a snapshot id. An edge additionally
    # records the name the dependent imports the package under, which differs
    # from the package's own name exactly when the spec is an npm alias.
    deps_by_sid = {}
    optional_by_sid = {}
    aliases_of_sid = {}
    for sid, snap in live.items():
        resolved = []
        optional = []
        for section in ("dependencies", "optionalDependencies"):
            for dep_name, dep_sid in snap[section].items():
                if dep_sid not in live or dep_sid == sid:
                    continue
                imported_as = dep_name if live[dep_sid]["name"] != dep_name else ""
                if imported_as:
                    aliases_of_sid.setdefault(dep_sid, {})[imported_as] = True
                resolved.append((dep_sid, imported_as))
                if section == "optionalDependencies":
                    optional.append(dep_sid)
        deps_by_sid[sid] = resolved
        optional_by_sid[sid] = optional

    # An alias the workspace itself imports is the one a consumer writes in a
    # BUILD file (`@npm//:h3-v2`), and no package in the graph declares it, so
    # without reading the importers for aliases the label would not exist.
    for imported_as, target in importers["aliases"].items():
        if target in live and live[target]["name"] != imported_as:
            aliases_of_sid.setdefault(target, {})[imported_as] = True

    # Cycle breaking works on resolved identity: an alias edge is judged by the
    # snapshot it reaches, not by the name it reaches it by.
    snapshot_graph = {sid: [dep_sid for (dep_sid, _) in edges] for sid, edges in deps_by_sid.items()}
    broken = break_cycles(snapshot_graph)
    dropped = {}
    for (frm, to) in broken:
        dropped.setdefault(frm, {})[to] = True

    # @types/<x> provides declarations for <x>. Prefer the @types major that
    # matches the runtime major, else the highest available.
    types_for = {}
    for sid, snap in live.items():
        if not snap["name"].startswith("@types/"):
            continue
        types_for.setdefault(snap["name"][len("@types/"):], []).append(sid)

    def _types_sid_for(snap):
        candidates = types_for.get(snap["name"], [])
        if not candidates:
            return None
        runtime_major = _semver_of(snap)[0]
        matching = [
            sid
            for sid in candidates
            if _semver_of(live[sid])[0] == runtime_major
        ]
        return _pick_primary(matching if matching else candidates, live, {})

    repo_of = {sid: _repo_name(hub_name, live[sid]) for sid in live}

    def _dep_label(dep_sid, imported_as):
        target = _alias_target_name(imported_as) if imported_as else "pkg"
        return "@{}//:{}".format(repo_of[dep_sid], target)

    for sid, snap in live.items():
        edges = [
            (_dep_label(dep_sid, imported_as), platforms_of[dep_sid])
            for (dep_sid, imported_as) in deps_by_sid[sid]
            if not dropped.get(sid, {}).get(dep_sid)
        ]
        deps, platform_deps = _split_by_platform(edges)

        # Platform-matched optionalDependencies are how npm ships native
        # sidecars (oxlint -> @oxlint/linux-x64-gnu). A bin script resolves them
        # through node_modules at runtime, and with one repository per package
        # they are no longer siblings on disk, so they have to be named as
        # targets. Not filtered by the cycle breaker: these are leaf binaries,
        # and this is a runfiles edge rather than a build edge.
        optional, platform_optional = _split_by_platform([
            ("@{}//:pkg".format(repo_of[dep_sid]), platforms_of[dep_sid])
            for dep_sid in optional_by_sid[sid]
        ])

        types_sid = None
        if not snap["name"].startswith("@types/"):
            types_sid = _types_sid_for(snap)

        patch = patches.get(snap["package_id"])
        resolution = packages[snap["package_id"]].get("resolution", {})
        local_tarball = _local_tarball_label(pnpm_lock, local_tarball_path(resolution))
        npm_import(
            name = repo_of[sid],
            package = snap["name"],
            version = snap["version"],
            peer_id = peer_suffix_dir_name(snap["peer_suffix"]),
            url = "" if local_tarball else npm_tarball_url(
                snap["name"],
                snap["version"],
                resolution,
                registries,
            ),
            local_tarball = local_tarball,
            integrity = resolution.get("integrity", ""),
            deps = deps,
            platform_deps = platform_deps,
            platforms = _ALL_PLATFORMS,
            types_dep = "@{}//:pkg".format(repo_of[types_sid]) if types_sid else "",
            store = _store_label(pnpm_lock, _snapshot_key(snap), snap["name"]),
            aliases = {
                _alias_target_name(alias): alias
                for alias in sorted(aliases_of_sid.get(sid, {}).keys())
            },
            npmrc = npmrc,
            patch = patch.label if patch else None,
            patch_sha256 = patch.sha256 if patch else "",
            optional_dep_packages = optional,
            platform_optional_dep_packages = platform_optional,
        )

    # ── The flat hub: the whole-lockfile namespace ────────────────────────────
    #
    # Aliases only, no downloads, so referencing one name fetches one package.
    # Bin targets are aliased lazily too -- the package's own BUILD file decides
    # whether a bin target exists, so the alias may dangle for a package with no
    # bin, which only errors if someone actually asks for it.
    root_importer = importers["importers"].get(".", {"deps": {}, "links": {}})
    root_sids = {sid: True for sid in root_importer["deps"].values()}

    sids_by_label = {}
    for sid, snap in live.items():
        sids_by_label.setdefault(package_name_to_label(snap["name"]), []).append(sid)

    aliases = {}
    for label, sids in sids_by_label.items():
        primary = _pick_primary(sids, live, root_sids)
        aliases[label] = "@{}//:pkg".format(repo_of[primary])
        aliases[label + "_bin"] = "@{}//:bin".format(repo_of[primary])

    # Version- and peer-qualified labels, so a consumer can name a resolution the
    # flat namespace had to pick between.
    for label, sids in sids_by_label.items():
        by_version = {}
        for sid in sids:
            by_version.setdefault(live[sid]["version"], []).append(sid)
        for version, version_sids in by_version.items():
            versioned = versioned_label_name(label, version)
            if versioned not in aliases:
                aliases[versioned] = "@{}//:pkg".format(repo_of[_pick_primary(version_sids, live, root_sids)])
            if len(version_sids) == 1:
                continue
            for sid in version_sids:
                peers = peer_suffix_dir_name(live[sid]["peer_suffix"]) or "no_peers"
                qualified = "{}__{}".format(versioned, peers)
                if qualified not in aliases:
                    aliases[qualified] = "@{}//:pkg".format(repo_of[sid])

    # An npm alias only gets a hub label when no real package owns that name --
    # a package actually called `h3-v2` is what a consumer means by `@npm//:h3-v2`.
    real_labels = {label: True for label in aliases}
    alias_owner = {}
    for sid, alias_names in aliases_of_sid.items():
        for alias in alias_names:
            label = package_name_to_label(alias)
            if label in real_labels:
                continue
            owner = alias_owner.get(label)
            if owner != None and owner != sid:
                # The hub is one flat namespace, so two importers aliasing the
                # same name at different packages has no right answer. Silently
                # keeping one would hand half the workspace the wrong package.
                fail(
                    "npm: the alias '{}' resolves to both {} and {} in this lockfile, ".format(
                        alias,
                        owner,
                        sid,
                    ) +
                    "so @{}//:{} cannot mean one of them.".format(hub_name, label),
                )
            alias_owner[label] = sid
            aliases[label] = "@{}//:{}".format(repo_of[sid], _alias_target_name(alias))

    # ── Workspace members: one view per member directory ─────────────────────
    # Every `link:` target and every named importer but the root.
    candidates = {}
    for name, path in importers["links"].items():
        if not path:
            continue
        if path in candidates and candidates[path] != name:
            fail(
                "npm: the workspace member at {} is linked as both '{}' and '{}' in this lockfile, ".format(
                    path,
                    candidates[path],
                    name,
                ) +
                "so @{} has no one view of it.".format(hub_name),
            )
        candidates[path] = name
    for path in importers["importers"]:
        if path != "." and path not in candidates:
            candidates[path] = None

    members = {}
    member_manifests = {}
    member_dirs = {}
    for path in sorted(candidates):
        manifest = _member_manifest(module_ctx, workspace_root, path)
        name = candidates[path]
        if not name and manifest:
            name = manifest.decoded["name"]
        if not name:
            continue
        label = package_name_to_label(name)
        if label in member_dirs:
            fail(
                "npm: the workspace members at {} and {} are both named '{}', ".format(
                    member_dirs[label],
                    path,
                    name,
                ) +
                "so @{}//:{} cannot mean one of them.".format(hub_name, label),
            )
        member_dirs[label] = path
        members[label] = _member_entry(name, path)
        if manifest:
            member_manifests[label] = manifest.text

        # The member IS what that name means; two targets of one name would not load.
        aliases.pop(label, None)

    # ── Per-importer packages: what each workspace member actually declared ───
    importer_aliases = {}
    for path, entry in importers["importers"].items():
        if path == ".":
            continue
        for dep_name, dep_sid in entry["deps"].items():
            if dep_sid not in live:
                continue
            label = package_name_to_label(dep_name)
            imported_as = dep_name if live[dep_sid]["name"] != dep_name else ""
            importer_aliases["{}|{}".format(path, label)] = _dep_label(dep_sid, imported_as)
            importer_aliases["{}|{}_bin".format(path, label)] = "@{}//:bin".format(repo_of[dep_sid])

    npm_hub(
        name = hub_name,
        pnpm_lock = pnpm_lock,
        aliases = aliases,
        importer_aliases = importer_aliases,
        members = members,
        member_manifests = member_manifests,
        store_graph = json.encode(_store_graph(
            pnpm_lock,
            struct(
                live = live,
                platforms_of = platforms_of,
                repo_of = repo_of,
                deps_by_sid = deps_by_sid,
                dropped = dropped,
                members = {
                    path: members[label].partition("|")[0]
                    for label, path in member_dirs.items()
                    if label in member_manifests
                },
            ),
            importers,
            settings,
        )),
        broken_cycle_edges = ["{} -> {}".format(a, b) for (a, b) in broken],
    )
