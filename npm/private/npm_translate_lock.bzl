"""pnpm lockfile reader: the whole-graph analysis behind the npm extension.

Everything here runs on lockfile text alone, with no network and no extracted
package, so npm/lazy.bzl can analyse the whole graph inside the module extension
and then declare one already-resolved repository per package.

What the lockfile has already decided, and we therefore must NOT re-derive:

  catalogs           every use site carries a concrete `version:`, so the
                     `catalogs:` block is dead weight.
  overrides          an override is a version SELECTION, so each outcome is
                     already its own `name@version` key in `packages:`.
  packageExtensions  injected peers appear in `snapshots:` as resolved
                     dependency edges, which _parse_snapshots ingests.

Each of those blocks is shaped exactly like the ones we do read -- `catalogs:`
entries look like importers, `overrides:` entries look like package keys -- so
every reader below anchors on its own `<section>:` header at indent 0 and stops
at the next indent-0 line. Nothing scans the file globally.

Two keyspaces, deliberately kept apart:

  packages:    one entry per published tarball, `name@version`. The bytes.
  snapshots:   one entry per RESOLUTION, `name@version(peer@version)...`. pnpm
               resolves a package once per distinct peer set and gives each
               outcome its own dependency edges, so this -- not `packages:` --
               is what a dependency graph may be keyed by.
  importers:   one entry per workspace member, naming the snapshot each of its
               declared dependencies resolved to. Two members can and do point
               at different snapshots of the same package name.

What the lockfile does NOT resolve is `patchedDependencies:`. The `packages:`
integrity is the byte-identical PRE-patch registry tarball, so the patch has to
be fetched and applied by whoever downloads the package.

Lockfile support: pnpm lockfile format v6 and v9.
"""

# ─── pnpm lockfile parsing ─────────────────────────────────────────────────────
#
# Starlark does not have a YAML parser built in, so we implement a minimal
# line-oriented parser sufficient for pnpm-lock.yaml format v6 and v9.
#
# pnpm-lock.yaml structure (simplified):
#
#   lockfileVersion: '6.0'
#
#   packages:
#     /react@19.0.0:
#       resolution: {integrity: sha512-...}
#       dependencies:
#         loose-envify: ^1.1.0
#
# In lockfile v9 the key format changes to `react@19.0.0` (no leading slash).

def _strip_leading_slash(s):
    if s.startswith("/"):
        return s[1:]
    return s

def _parse_package_key(key):
    """Parses a pnpm lockfile package key into (name, version).

    Handles both v6 (/name@version) and v9 (name@version) formats.
    Handles scoped packages: /@scope/name@version or @scope/name@version.
    Handles pnpm v9 format where scoped package keys are quoted:
      '@types/estree@1.0.8' → ("@types/estree", "1.0.8")
    Handles pnpm v9 peer-dependency suffixes:
      react-dom@19.0.0(react@19.0.0) → ("react-dom", "19.0.0")
    """
    key = key.strip()

    # Strip surrounding single quotes added by pnpm v9 for scoped packages.
    if key.startswith("'") and key.endswith("'"):
        key = key[1:-1]
    key = _strip_leading_slash(key)

    # Strip pnpm v9 peer-dependency suffix before parsing name@version.
    # e.g. "react-dom@19.0.0(react@19.0.0)" → "react-dom@19.0.0"
    paren_idx = key.find("(")
    if paren_idx != -1:
        key = key[:paren_idx]
    if key.startswith("@"):
        # scoped: @scope/name@version
        slash_idx = key.find("/")
        if slash_idx == -1:
            return (key, "")
        rest = key[slash_idx + 1:]  # "name@version"
        at_idx = rest.rfind("@")
        if at_idx == -1:
            return (key, "")
        pkg_name = key[:slash_idx + 1 + at_idx]  # "@scope/name"
        version = rest[at_idx + 1:]
        return (pkg_name, version)
    else:
        at_idx = key.rfind("@")
        if at_idx == -1:
            return (key, "")
        return (key[:at_idx], key[at_idx + 1:])

def _indent_level(line):
    """Returns the number of leading spaces in a line."""
    count = 0
    for ch in line.elems():
        if ch == " ":
            count += 1
        else:
            break
    return count

def _new_pkg_entry():
    """Returns a fresh mutable dict for a package entry."""
    return {
        "name": "",
        "version": "",
        "published_version": "",
        "resolution": {},
        "dependencies": {},
        "optionalDependencies": {},
        "peerDependencies": {},
        "os": [],
        "cpu": [],
        "libc": [],
    }

def _record_package(packages, current_pkg_key, current_pkg):
    """Records current_pkg into packages if current_pkg_key is set.

    Records ALL packages including those with os/cpu constraints.  Platform
    filtering happens in the caller, once the host platform is known.
    """
    if current_pkg_key:
        name, version = _parse_package_key(current_pkg_key)
        if not name or not version:
            return
        pkg_id = "{}@{}".format(name, version)
        entry = dict(current_pkg)
        entry["name"] = name
        entry["version"] = version
        packages[pkg_id] = entry

def _parse_inline_resolution(value):
    """Parses an inline YAML mapping like '{integrity: sha512-...}' into a dict."""
    inner = value.strip()
    if inner.startswith("{"):
        inner = inner[1:]
    if inner.endswith("}"):
        inner = inner[:-1]
    result = {}
    for pair in inner.split(","):
        pair = pair.strip()
        if ":" in pair:
            rk, _, rv = pair.partition(":")
            result[rk.strip()] = rv.strip().strip("'\"")
    return result

def _find_lockfile_version(lines):
    """Scans lines for lockfileVersion and returns it as a string."""
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("lockfileVersion:"):
            parts = stripped.split(":", 1)
            return parts[1].strip().strip("'\"")
    return ""

def _find_packages_start(lines):
    """Returns the index of the 'packages:' section header, or -1."""
    for idx in range(len(lines)):
        if lines[idx].rstrip() == "packages:":
            return idx
    return -1

def _find_snapshots_start(lines):
    """Returns the index of the 'snapshots:' section header, or -1."""
    for idx in range(len(lines)):
        if lines[idx].rstrip() == "snapshots:":
            return idx
    return -1

def _snapshot_parts(key):
    """Splits a snapshots: key into (package_id, peer_suffix).

    'foo@1.0.0(react@18.3.1)' -> ("foo@1.0.0", "(react@18.3.1)")

    The package_id is what `packages:` is keyed by -- the published tarball, one
    per name@version. The peer suffix is what makes this ONE RESOLUTION of that
    tarball: pnpm resolves a package once per distinct peer set and writes each
    outcome as its own snapshot with its own dependency edges.
    """
    key = key.strip()
    if key.startswith("'") and key.endswith("'"):
        key = key[1:-1]
    key = _strip_leading_slash(key)
    paren = key.find("(")
    if paren == -1:
        return (key, "")
    return (key[:paren], key[paren:])

_TARBALL_SUFFIXES = (".tgz", ".tar.gz")

def _is_file_tarball(value):
    """A `file:` dependency on an archive, which pnpm locks by integrity like a registry package."""
    return value.startswith("file:") and value.endswith(_TARBALL_SUFFIXES)

def _local_tarball_path(resolution):
    """The lockfile-relative path of a `file:` tarball, or "" for any other resolution."""
    tarball = resolution.get("tarball", "")
    return tarball[len("file:"):] if _is_file_tarball(tarball) else ""

def _dep_snapshot_id(dep_name, value):
    """The snapshots: key that a dependency value names, or "" for a non-registry dep.

    pnpm writes the value relative to the dependency NAME -- `3.0.9(vite@6.4.1)`
    under `vitest:` -- so the key has to be reassembled. An npm alias is the one
    exception: its value already carries the aliased package's own name, which is
    what makes it the whole key.
    """
    value = value.strip().strip("'\"")
    if not value or value.startswith("link:"):
        return ""
    if value.startswith("file:"):
        return "{}@{}".format(dep_name, value) if _is_file_tarball(value) else ""
    if _alias_target(value):
        return value
    return "{}@{}".format(dep_name, value)

def _new_snapshot_entry(sid):
    package_id, peer_suffix = _snapshot_parts(sid)
    name, version = _parse_package_key(package_id)
    return {
        "id": sid,
        "package_id": package_id,
        "peer_suffix": peer_suffix,
        "name": name,
        "version": version,
        "dependencies": {},
        "optionalDependencies": {},
    }

def _parse_snapshots(lines, snapshots_start):
    """Parses the snapshots: section into {snapshot_id: entry}.

    A snapshot is a package AS RESOLVED for one peer set, and it is the unit the
    dependency graph is actually built out of. Collapsing snapshots onto their
    `packages:` key -- which is what stripping the peer suffix does -- merges the
    dependency edges of resolutions that were deliberately kept apart, and the
    merge is silent: every version involved is real, so nothing downstream can
    tell that an importer was handed the other variant's transitive closure.

    Args:
        lines:           All lines of the lockfile.
        snapshots_start: Line index of the 'snapshots:' header.

    Returns:
        dict: {snapshot_id: {"id", "package_id", "peer_suffix", "name",
               "version", "dependencies": {dep_name: dep_snapshot_id},
               "optionalDependencies": {...}}}
    """
    snapshots = {}
    state = {"sid": None, "section": None, "done": False}

    for idx in range(snapshots_start + 1, len(lines)):
        if state["done"]:
            break

        raw_line = lines[idx].rstrip()
        if not raw_line or raw_line.strip().startswith("#"):
            continue

        indent = _indent_level(raw_line)
        stripped = raw_line.strip()

        if indent == 0:
            state["done"] = True
            continue

        # Snapshot key. `foo@1.0.0: {}` is the same key with an inline empty
        # mapping, and the entry still has to exist -- a package with no
        # dependencies is a node in the graph.
        if indent == 2:
            if stripped.endswith(":"):
                raw_key = stripped[:-1]
            elif ": " in stripped:
                # A YAML key ends at ": "; a `file:` key carries its own colon.
                raw_key = stripped.partition(": ")[0]
            else:
                continue
            package_id, peer_suffix = _snapshot_parts(raw_key)
            sid = package_id + peer_suffix
            entry = _new_snapshot_entry(sid)
            if not entry["name"] or not entry["version"]:
                state["sid"] = None
                continue
            snapshots[sid] = entry
            state["sid"] = sid
            state["section"] = None
            continue

        if state["sid"] == None:
            continue

        if indent == 4 and stripped.endswith(":") and ":" not in stripped[:-1]:
            section = stripped[:-1]
            state["section"] = section if section in ("dependencies", "optionalDependencies") else None
            continue

        if indent == 6 and ":" in stripped and state["section"]:
            kv_k, _, kv_v = stripped.partition(":")
            dep_name = kv_k.strip().strip("'\"")
            dep_sid = _dep_snapshot_id(dep_name, kv_v)
            if dep_sid:
                snapshots[state["sid"]][state["section"]][dep_name] = dep_sid
            continue

    return snapshots

def _snapshots_from_packages(packages):
    """Synthesises snapshots for a v6 lockfile, where deps live in `packages:`.

    v6 has no `snapshots:` section: each `packages:` key IS the resolution, peer
    decoration included, so the two collapse into one node per key.
    """
    snapshots = {}
    for pkg_id, pkg in packages.items():
        entry = _new_snapshot_entry(pkg_id)
        entry["name"] = pkg["name"]
        entry["version"] = pkg["version"]
        for section in ("dependencies", "optionalDependencies"):
            for dep_name, dep_value in pkg.get(section, {}).items():
                dep_sid = _dep_snapshot_id(dep_name, dep_value)
                if dep_sid:
                    entry[section][dep_name] = dep_sid
        snapshots[pkg_id] = entry
    return snapshots

def _parse_pnpm_lock(content):
    """Parses pnpm-lock.yaml content into a dict of package metadata.

    Handles both pnpm lockfile v6 (dependencies in packages: section) and
    v9 (dependencies in snapshots: section; packages: only has resolution info).

    `packages:` and `snapshots:` answer two different questions and are both
    returned: `packages:` is keyed by the published tarball (one entry per
    name@version, carrying resolution/os/cpu/peerDependencies), `snapshots:` by
    the RESOLUTION (one entry per name@version per peer set, carrying the
    dependency edges). The graph is built from snapshots; packages supplies the
    bytes to download.

    Returns:
        dict: {
            "lockfile_version": str,
            "packages": {
                "name@version": {
                    "name": str,
                    "version": str,
                    "resolution": {integrity: str, tarball: str, ...},
                    "dependencies": {name: version_spec, ...},     # v6 only
                    "optionalDependencies": {name: version_spec, ...},  # v6 only
                    "peerDependencies": {name: range, ...},
                    "os": [str, ...],
                    "cpu": [str, ...],
                },
            },
            "snapshots": {
                "name@version(peer@version)": {
                    "id": str, "package_id": str, "peer_suffix": str,
                    "name": str, "version": str,
                    "dependencies": {dep_name: dep_snapshot_id, ...},
                    "optionalDependencies": {dep_name: dep_snapshot_id, ...},
                },
            },
        }
    """
    lines = content.split("\n")
    lockfile_version = _find_lockfile_version(lines)
    packages = {}

    packages_start = _find_packages_start(lines)
    if packages_start == -1:
        return {"lockfile_version": lockfile_version, "packages": packages, "snapshots": {}}

    # Parse package entries using a for loop with a mutable state dict.
    # Starlark forbids while loops, so we track state as a single mutable dict.
    state = {
        "current_pkg_key": None,
        "current_section": None,
        "current_pkg": _new_pkg_entry(),
        "done": False,
    }

    for idx in range(packages_start + 1, len(lines)):
        if state["done"]:
            break

        line = lines[idx]
        raw_line = line.rstrip()

        # Skip blank lines and comments.
        if not raw_line or raw_line.startswith("#"):
            continue

        indent = _indent_level(raw_line)
        stripped = raw_line.strip()

        # Detect end of packages section: a top-level key (indent 0).
        if indent == 0 and not raw_line.startswith(" "):
            if stripped != "packages:":
                _record_package(packages, state["current_pkg_key"], state["current_pkg"])
                state["current_pkg_key"] = None
                state["done"] = True
            continue

        # Package key: indent 2, ends with colon.
        if indent == 2 and stripped.endswith(":"):
            _record_package(packages, state["current_pkg_key"], state["current_pkg"])
            state["current_pkg_key"] = stripped[:-1]
            state["current_pkg"] = _new_pkg_entry()
            state["current_section"] = None
            continue

        # Skip if we haven't started a package block.
        if state["current_pkg_key"] == None:
            continue

        # Section header inside a package block (indent 4, ends with colon,
        # no value on same line).
        if indent == 4 and stripped.endswith(":") and ":" not in stripped[:-1]:
            section_name = stripped[:-1]
            if section_name in ("resolution", "dependencies", "optionalDependencies", "peerDependencies", "engines"):
                state["current_section"] = section_name
            else:
                state["current_section"] = None
            continue

        # Key-value at indent 4 (package-level field or inline resolution).
        if indent == 4 and ":" in stripped and not stripped.endswith(":"):
            kv_k, _, kv_v = stripped.partition(":")
            kv_k = kv_k.strip()
            kv_v = kv_v.strip().strip("'\"")
            if kv_k == "resolution":
                state["current_pkg"]["resolution"] = _parse_inline_resolution(kv_v)
                state["current_section"] = None
            elif kv_k in ("os", "cpu", "libc"):
                # e.g. "os: [darwin, linux]", "cpu: [x64]", "libc: [musl]".
                inner = kv_v.strip().strip("[]")
                state["current_pkg"][kv_k] = [x.strip() for x in inner.split(",") if x.strip()]
            elif kv_k == "version":
                # A `file:` package is keyed by its path; pnpm records its own version here.
                state["current_pkg"]["published_version"] = kv_v
            continue

        # Key-value at indent 6 (inside a section).
        if indent == 6 and ":" in stripped and state["current_section"]:
            kv_k, _, kv_v = stripped.partition(":")
            kv_k = kv_k.strip()
            kv_v = kv_v.strip().strip("'\"")
            if state["current_section"] in ("dependencies", "optionalDependencies", "peerDependencies"):
                state["current_pkg"][state["current_section"]][kv_k] = kv_v
            elif state["current_section"] == "resolution":
                state["current_pkg"]["resolution"][kv_k] = kv_v
            continue

    # Flush the last package (if we exhausted all lines without hitting a new top-level key).
    if not state["done"]:
        _record_package(packages, state["current_pkg_key"], state["current_pkg"])

    snapshots_start = _find_snapshots_start(lines)
    if snapshots_start != -1:
        snapshots = _parse_snapshots(lines, snapshots_start)
    else:
        snapshots = _snapshots_from_packages(packages)

    for snap in snapshots.values():
        published = packages.get(snap["package_id"], {}).get("published_version", "")
        if published:
            snap["semver"] = published

    return {
        "lockfile_version": lockfile_version,
        "packages": packages,
        "snapshots": snapshots,
    }

# ─── pnpm importers: workspace links and npm aliases ─────────────────────────
#
# `importers:` lists every workspace member by its path relative to the
# lockfile, and for each declared dependency records the specifier the member
# asked for and the version pnpm resolved it to:
#
#   importers:
#     .:
#       dependencies:
#         shared:          {specifier: workspace:*,      version: link:packages/shared}
#         tailwindcss-v3:  {specifier: npm:tailwindcss@3.4.18, version: tailwindcss@3.4.18}
#         zod:             {specifier: ^3.0.0,           version: 3.24.2}
#
# Two of the three forms matter to us, and the `version:` value alone tells
# them apart -- the specifier is never needed:
#
#   link:<path>   a workspace member. The path is relative to the IMPORTER, not
#                 to the lockfile, so it is resolved against the importer path
#                 before it can become a "//<path>:<name>" label.
#   <name>@<ver>  an npm alias: the member imports the package under a name
#                 that is not the package's own. A plain resolved version never
#                 contains "@", so an "@" past index 0 (index 0 being a scope,
#                 as in '@typescript/typescript6@6.0.2') is the discriminator.
#
# Peer suffixes are stripped before that test: `1.2.3(react@19.0.0)` is a plain
# version whose peer decoration would otherwise look like an alias. Stripped for
# the test only -- what is recorded is the snapshot the value names, peers and
# all, because that is the key the graph is built on.
#
# This reader anchors on `importers:` and stops at the next indent-0 line. It
# must stay that way: `catalogs:` is the same nesting idiom one level shallower,
# with catalog names sitting at indent 2 exactly where importer paths do and the
# same `specifier:`/`version:` pairs underneath, so a reader that scanned for
# those pairs instead of anchoring would pull catalog entries in as importers.

def _find_importers_start(lines):
    """Returns the index of the 'importers:' section header, or -1."""
    for idx in range(len(lines)):
        if lines[idx].rstrip() == "importers:":
            return idx
    return -1

def _resolve_link_path(importer_path, link_path):
    """Resolves a pnpm `link:` target into a path relative to the workspace root.

    pnpm records link targets relative to the importer that declares them, so
    importer "npm-packages/email-js" with `link:../../packages/api-utils`
    refers to "packages/api-utils". The root importer is spelled ".".

    Returns "" when the link resolves to the workspace root itself or escapes
    it -- neither can be expressed as a "//<path>:<name>" label.
    """
    segments = []
    for part in (importer_path + "/" + link_path).split("/"):
        if not part or part == ".":
            continue
        if part == "..":
            if not segments:
                return ""
            segments = segments[:-1]
            continue
        segments.append(part)
    return "/".join(segments)

def _strip_peer_suffix(version):
    """Drops a pnpm peer/patch decoration: 'h3@2.0.1(crossws@0.4.5)' -> 'h3@2.0.1'."""
    paren = version.find("(")
    return version[:paren] if paren != -1 else version

def _alias_target(version):
    """Returns the 'name@version' an alias resolves to, or "" for a plain version.

    A resolved version is bare digits and dots; only an alias carries the
    package name. Index 0 is skipped so a scope's '@' is not the one we find.
    """
    version = _strip_peer_suffix(version)
    if len(version) > 1 and version.find("@", 1) != -1:
        return version
    return ""

def _importer_entry(result, importer):
    if importer not in result["importers"]:
        result["importers"][importer] = {"deps": {}, "links": {}}
    return result["importers"][importer]

def _record_importer_dep(result, importer, dep_name, version):
    """Records one importer dependency: a workspace link, or a snapshot it resolved to.

    The per-importer map is the only place the lockfile's per-member resolution
    survives. The flat `links`/`aliases` maps are a union across all members and
    therefore lossy by construction -- two members declaring different majors of
    one package both land on the same key.
    """
    entry = _importer_entry(result, importer)
    if version.startswith("link:"):
        path = _resolve_link_path(importer, version[len("link:"):])
        if path:
            result["links"][dep_name] = path
            entry["links"][dep_name] = path
        return
    dep_sid = _dep_snapshot_id(dep_name, version)
    if not dep_sid:
        return
    entry["deps"][dep_name] = dep_sid
    if _alias_target(version):
        result["aliases"][dep_name] = dep_sid

def _parse_importers(content):
    """Parses the importers: section into workspace links and npm aliases.

    Returns:
        dict: {
            "links":     {npm_package_name: workspace_rel_path},
            "aliases":   {imported_name: snapshot_id},
            "importers": {importer_path: {
                "deps":  {dep_name: snapshot_id},
                "links": {dep_name: workspace_rel_path},
            }},
        }
    """
    lines = content.split("\n")
    importers_start = _find_importers_start(lines)
    result = {"links": {}, "aliases": {}, "importers": {}}
    if importers_start == -1:
        return result

    state = {
        "current_importer": ".",
        "current_section": None,
        "current_dep_name": None,
        "done": False,
    }

    for idx in range(importers_start + 1, len(lines)):
        if state["done"]:
            break

        raw_line = lines[idx].rstrip()
        if not raw_line or raw_line.strip().startswith("#"):
            continue

        indent = _indent_level(raw_line)
        stripped = raw_line.strip()

        if indent == 0:
            state["done"] = True
            continue

        # Importer entry: a path like "." or "packages/shared". `dir: {}` is an
        # importer that declares nothing, and a workspace member all the same.
        if indent == 2:
            if stripped.endswith(":"):
                importer = stripped[:-1]
            elif ":" in stripped:
                importer = stripped.partition(":")[0]
            else:
                continue
            state["current_importer"] = importer.strip().strip("'\"")
            _importer_entry(result, state["current_importer"])
            state["current_section"] = None
            state["current_dep_name"] = None
            continue

        if indent == 4 and stripped.endswith(":") and ":" not in stripped[:-1]:
            state["current_section"] = stripped[:-1]
            state["current_dep_name"] = None
            continue

        if state["current_section"] not in ("dependencies", "devDependencies", "optionalDependencies"):
            continue

        if indent == 6:
            # v6 inline form: `shared: {specifier: workspace:*, version: link:packages/shared}`
            if ":" in stripped and "{" in stripped:
                dep_name, _, rest = stripped.partition(":")
                dep_name = dep_name.strip().strip("'\"")
                marker = "version:"
                pos = rest.find(marker)
                if dep_name and pos != -1:
                    value = rest[pos + len(marker):].split("}")[0].split(",")[0].strip().strip("'\"")
                    _record_importer_dep(result, state["current_importer"], dep_name, value)
                state["current_dep_name"] = None
                continue

            # v9 form: the dep name on its own line, specifier/version below it.
            if stripped.endswith(":"):
                state["current_dep_name"] = stripped[:-1].strip().strip("'\"")
                continue

        if indent == 8 and state["current_dep_name"] and ":" in stripped:
            kv_k, _, kv_v = stripped.partition(":")
            if kv_k.strip() == "version":
                _record_importer_dep(
                    result,
                    state["current_importer"],
                    state["current_dep_name"],
                    kv_v.strip().strip("'\""),
                )
            continue

    return result

# ─── patchedDependencies ─────────────────────────────────────────────────────
#
# The one pnpm feature the resolved graph does NOT carry. A patched package's
# `packages:` entry holds the integrity of the untouched registry tarball, so
# fetching it faithfully gets you the UNPATCHED package; only the `snapshots:`
# key records `(patch_hash=...)` and that is decoration, not content.
#
# Two spellings, both keyed by `name@version`:
#
#   patchedDependencies:               patchedDependencies:
#     '@acme/diffs@1.3.1': <sha256>    '@acme/diffs@1.3.1':
#                                          hash: <sha256>
#                                          path: patches/...
#
# The hash is the plain sha256 of the patch file's bytes, which makes it a
# usable content check
# on the patch we are about to apply -- see npm_import's patch handling. The
# `path:` is deliberately ignored: a lockfile-relative path cannot be turned
# into a label without knowing where Bazel package boundaries fall, so patches
# arrive as real labels from the extension's `patches` attribute instead.

def _find_patched_dependencies_start(lines):
    """Returns the index of the 'patchedDependencies:' header, or -1."""
    for idx in range(len(lines)):
        if lines[idx].rstrip() == "patchedDependencies:":
            return idx
    return -1

def _parse_patched_dependencies(content):
    """Parses patchedDependencies: into {"name@version": sha256_of_patch_file}.

    An entry whose hash is missing maps to "", which callers must read as
    "patched, but nothing to verify the patch file against".
    """
    lines = content.split("\n")
    start = _find_patched_dependencies_start(lines)
    if start == -1:
        return {}

    patched = {}
    state = {"current_key": None, "done": False}

    for idx in range(start + 1, len(lines)):
        if state["done"]:
            break

        raw_line = lines[idx].rstrip()
        if not raw_line or raw_line.strip().startswith("#"):
            continue

        indent = _indent_level(raw_line)
        stripped = raw_line.strip()

        if indent == 0:
            state["done"] = True
            continue

        if indent == 2:
            key, _, value = stripped.partition(":")
            key = key.strip().strip("'\"")
            if not key:
                continue
            state["current_key"] = key
            patched[key] = value.strip().strip("'\"")
            continue

        if indent == 4 and state["current_key"] != None:
            kv_k, _, kv_v = stripped.partition(":")
            if kv_k.strip() == "hash":
                patched[state["current_key"]] = kv_v.strip().strip("'\"")
            continue

    return patched

# ─── Which registry a tarball comes from ──────────────────────────────────────

DEFAULT_NPM_REGISTRY = "https://registry.npmjs.org"

def _npmrc_assignments(content):
    """Every `key=value` in an .npmrc, with comments and section headers dropped."""
    out = []
    for raw in content.split("\n"):
        line = raw.strip()
        if not line or line.startswith("#") or line.startswith(";") or line.startswith("["):
            continue
        key, sep, value = line.partition("=")
        if not sep:
            continue
        out.append((key.strip(), value.strip().strip("'\"")))
    return out

def _normalise_registry(url):
    """A registry URL with no trailing slash, and a scheme even if npm omitted it."""
    if url.startswith("//"):
        url = "https:" + url
    return url.rstrip("/")

def _npmrc_registries(content):
    """{"": default_url, "@scope": url, ...} from an .npmrc.

    An empty result means the file names no registry, which callers read as "the
    public registry" -- not as "no registry".
    """
    registries = {}
    for (key, value) in _npmrc_assignments(content):
        if key == "registry":
            registries[""] = _normalise_registry(value)
        elif key.startswith("@") and key.endswith(":registry"):
            registries[key[:-len(":registry")]] = _normalise_registry(value)
    return registries

def _yaml_scalar(text):
    """A YAML plain or quoted scalar, its trailing comment dropped."""
    text = text.strip()
    if text[:1] in ('"', "'"):
        end = text.find(text[0], 1)
        return text[1:end] if end != -1 else text[1:]
    comment = text.find(" #")
    return (text[:comment] if comment != -1 else text).strip()

def _yaml_pair(line):
    """(key, value) of a `key: value` line, or (None, None) for any other."""
    text = line.strip()
    if text[:1] in ('"', "'"):
        end = text.find(text[0], 1)
        if end == -1 or not text[end + 1:].lstrip().startswith(":"):
            return (None, None)
        return (text[1:end], _yaml_scalar(text[end + 1:].lstrip()[1:]))
    key, sep, value = text.partition(":")
    if not sep or not key.strip():
        return (None, None)
    return (key.strip(), _yaml_scalar(value))

def _pnpm_workspace_registries(content):
    """{"": default_url, "@scope": url, ...} from a pnpm-workspace.yaml.

    `registries:` maps `default` and `@scope` keys to URLs; a top-level
    `registry:` sets the default over the block's. A value naming `${VAR}` is
    dropped, as pnpm drops it from a project's file.
    """
    registries = {}
    default = None
    in_block = False
    for raw in content.split("\n"):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, value = _yaml_pair(raw)
        if not raw[0].isspace():
            in_block = key == "registries"
            if in_block and value and not value.startswith("#"):
                fail("pnpm-workspace.yaml: use an indented registries mapping with default or @scope keys; inline mappings and aliases are unsupported")
            if key == "registry" and value and "${" not in value:
                default = value
            continue
        if not in_block:
            continue
        if key == None or (key != "default" and not key.startswith("@")):
            fail("pnpm-workspace.yaml: use an indented registries mapping with default or @scope keys; inline mappings and aliases are unsupported")
        if "${" in value:
            continue
        if key == "default":
            registries[""] = _normalise_registry(value)
        elif key.startswith("@"):
            registries[key] = _normalise_registry(value)
    if default != None:
        registries[""] = _normalise_registry(default)
    return registries

def pnpm_workspace_hoist_settings(content):
    patterns = {"hoistPattern": "hoist-pattern", "publicHoistPattern": "public-hoist-pattern"}
    flags = {"hoist": "hoist", "hoistWorkspacePackages": "hoist-workspace-packages"}
    settings = {}
    block = None
    for raw in (content or "").split("\n"):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if block != None and line.startswith("- "):
            settings[block].append(_yaml_scalar(line[2:]))
            continue
        if raw[0].isspace():
            if block != None:
                fail("pnpm-workspace.yaml: {} needs a list of patterns. Did you mean an indented '- pattern' entry?".format(block))
            continue
        block = None
        key, value = _yaml_pair(raw)
        if key in patterns:
            name = patterns[key]
            if not value or value.startswith("#"):
                settings[name] = []
                block = name
            elif value.startswith("["):
                entries = json.decode(value)
                if type(entries) != "list" or any([type(entry) != "string" for entry in entries]):
                    fail("pnpm-workspace.yaml: {} needs string patterns. Did you mean a block list of quoted patterns?".format(key))
                settings[name] = entries
            else:
                fail("pnpm-workspace.yaml: {} needs a block list or JSON string array. Did you mean an indented '- pattern' entry?".format(key))
        elif key in flags:
            if value not in ("true", "false"):
                fail("pnpm-workspace.yaml: {} needs a boolean. Did you mean true or false?".format(key))
            settings[flags[key]] = value == "true"
    return settings

def _workspace_registries(npmrc, pnpm_workspace):
    """The registry map a workspace's two files settle on, {"": default,
    "@scope": url}: the .npmrc's lines, then pnpm-workspace.yaml's, the later
    winning a key -- the order pnpm 11's config reader builds it in.

    Args:
        npmrc: The .npmrc text, or "" without one.
        pnpm_workspace: The pnpm-workspace.yaml text, or "" without one.

    Returns:
        {} when neither names a registry, which callers read as npmjs.
    """
    registries = _npmrc_registries(npmrc or "")
    registries.update(_pnpm_workspace_registries(pnpm_workspace or ""))
    return registries

def _package_scope(package_name):
    """'@types' for '@types/react', '' for 'react'."""
    if not package_name.startswith("@"):
        return ""
    slash = package_name.find("/")
    return package_name[:slash] if slash != -1 else ""

def _registry_for(package_name, registries):
    """The registry a package's tarball lives on, scope override included."""
    scope = _package_scope(package_name)
    if scope in registries:
        return registries[scope]
    return registries.get("", DEFAULT_NPM_REGISTRY)

# Bazel verifies a download against an SRI hash, and pnpm writes one per
# published tarball. The algorithms are named explicitly rather than left to
# Bazel's checksum parser: a digest it would refuse (`sha1-` from a lockfile old
# enough to have one) is then reported here, naming the package, instead of
# arriving as a checksum error naming a URL.
_INTEGRITY_ALGORITHMS = ("sha512-", "sha384-", "sha256-")

def _verify_integrity(packages):
    """The `packages:` entries whose resolution carries no usable integrity.

    Pure, so the whole-lockfile check can be unit-tested without a fetch.

    Args:
        packages: The `packages` map from parse_pnpm_lock.

    Returns:
        A list of struct(package_id, resolution), ordered by package_id. Empty
        means every entry can be checked against the bytes it names.
    """
    unverifiable = []
    for package_id in sorted(packages):
        resolution = packages[package_id].get("resolution", {})
        integrity = resolution.get("integrity", "")
        usable = False
        for algorithm in _INTEGRITY_ALGORITHMS:
            if integrity.startswith(algorithm):
                usable = True
        if not usable:
            unverifiable.append(struct(package_id = package_id, resolution = resolution))
    return unverifiable

def _npm_tarball_url(package_name, version, resolution, registries = {}):
    """Returns the tarball URL for an npm package.

    Args:
        package_name: e.g. '@types/react'.
        version:      The resolved version.
        resolution:   The lockfile's `resolution:` mapping. A `tarball:` there is
                      an absolute URL pnpm already resolved (a git or http
                      dependency), and it wins over any registry.
        registries:   The {"": default, "@scope": url} map, or {}.
    """
    if "tarball" in resolution:
        return resolution["tarball"]

    registry = _registry_for(package_name, registries)
    if package_name.startswith("@"):
        # Scoped package: @scope/name
        scope, _, name = package_name[1:].partition("/")
        return "{registry}/@{scope}/{name}/-/{name}-{version}.tgz".format(
            registry = registry,
            scope = scope,
            name = name,
            version = version,
        )
    return "{registry}/{name}/-/{name}-{version}.tgz".format(
        registry = registry,
        name = package_name,
        version = version,
    )

# ─── Label helpers ─────────────────────────────────────────────────────────────

def _package_name_to_label(package_name):
    """Converts an npm package name to a valid Bazel label name component.

    '@types/react' → 'types_react'
    'react-dom'    → 'react-dom'
    """
    name = package_name
    if name.startswith("@"):
        name = name[1:]
    name = name.replace("/", "_")
    return name

def _version_name(version):
    """A version as a name component; a `file:` version also carries a path."""
    for ch in (".", "+", "-", ":", "/"):
        version = version.replace(ch, "_")
    return version

def _package_dir_name(package_name, version):
    """Returns the subdirectory name for an extracted package inside the @npm repo.

    '@types/react' + '19.0.0' → 'types_react__19_0_0'
    """
    label = _package_name_to_label(package_name)
    version_clean = _version_name(version)
    return "{}__{}".format(label, version_clean)

def _versioned_label_name(base_label, version):
    """Returns the versioned Bazel label name for a package.

    e.g. ("vitest_pretty-format", "3.0.9") → "vitest_pretty-format_3_0_9"
         ("react", "19.1.0")               → "react_19_1_0"

    The base_label is the result of _package_name_to_label, and the version
    component replaces dots and hyphens with underscores to produce a valid
    Bazel label name component.
    """
    return "{}_{}".format(base_label, _version_name(version))

_DIGIT_VALUES = {
    "0": 0,
    "1": 1,
    "2": 2,
    "3": 3,
    "4": 4,
    "5": 5,
    "6": 6,
    "7": 7,
    "8": 8,
    "9": 9,
}

def _semver_parts(v):
    """Parses a semver string into [major, minor, patch, prerelease_flag, prerelease_ids].

    major.minor.patch is always exactly three integers: a missing component reads
    as 0, a fourth one ("1.2.3.4") is dropped, and build metadata ("+build.5") is
    dropped because semver gives it no precedence.

    prerelease_flag is 0 for a stable release and 1 for a pre-release, so that
    "19.0.0-rc.1" ranks below "19.0.0". prerelease_ids holds one sort key per
    dot-separated pre-release identifier, in the order semver compares them:
    (0, number, "") for a numeric identifier and (1, 0, text) for an alphanumeric
    one, which outranks every numeric identifier.
    """

    def leading_int(s):
        n = 0
        for c in s.elems():
            if c not in _DIGIT_VALUES:
                break
            n = n * 10 + _DIGIT_VALUES[c]
        return n

    def sort_key(ident):
        for c in ident.elems():
            if c not in _DIGIT_VALUES:
                return (1, 0, ident)
        return (0, leading_int(ident), "")

    core = v.split("+")[0]
    dash = core.find("-")
    numeric = core[:dash] if dash != -1 else core
    prerelease = core[dash + 1:] if dash != -1 else ""

    result = [leading_int(p) for p in numeric.split(".")[:3]]
    for _ in range(3 - len(result)):
        result.append(0)

    result.append(1 if prerelease else 0)
    result.append([sort_key(ident) for ident in prerelease.split(".")] if prerelease else [])
    return result

def _semver_gt(a_parts, b_parts):
    """Returns True if semver a_parts is strictly greater than b_parts.

    Compares major, minor, patch numerically, then ranks a stable release above
    any pre-release of the same core version, then compares pre-release
    identifiers pairwise, a prefix ranking below the longer list it prefixes.
    """
    for i in range(3):
        if a_parts[i] > b_parts[i]:
            return True
        if a_parts[i] < b_parts[i]:
            return False

    if a_parts[3] != b_parts[3]:
        return a_parts[3] < b_parts[3]

    a_ids = a_parts[4]
    b_ids = b_parts[4]
    for i in range(min(len(a_ids), len(b_ids))):
        if a_ids[i] != b_ids[i]:
            return a_ids[i] > b_ids[i]
    return len(a_ids) > len(b_ids)

# ─── Platform constraints ─────────────────────────────────────────────────────
#
# `os:`/`cpu:`/`libc:` on a `packages:` entry says which platforms the published
# tarball is FOR. Reading the host to decide which of them to declare would bake
# the machine that ran the module extension into MODULE.bazel.lock -- a file that
# is committed and shared. So nothing here looks at the host: every platform's
# packages are declared, and the choice is a select() resolved at analysis time.

def _pkg_matches_platform(pkg, npm_os, npm_cpu, npm_libc):
    """Whether a package's os/cpu/libc constraints admit the given platform.

    An absent constraint admits everything; every one present must match.

    Args:
        pkg:      A `packages:` entry from _parse_pnpm_lock.
        npm_os:   pnpm's name for the OS ("linux", "darwin", "win32", ...).
        npm_cpu:  pnpm's name for the CPU ("x64", "arm64", ...).
        npm_libc: pnpm's name for the platform's libc ("glibc"), "" off linux.

    Returns:
        bool
    """
    pkg_os = pkg.get("os", [])
    pkg_cpu = pkg.get("cpu", [])
    pkg_libc = pkg.get("libc", [])
    if pkg_os and npm_os not in pkg_os:
        return False
    if pkg_cpu and npm_cpu not in pkg_cpu:
        return False
    if pkg_libc and npm_libc not in pkg_libc:
        return False
    return True

# ─── Snapshot identity as a directory name ────────────────────────────────────

_MASK32 = 4294967295
_HEX_DIGITS = "0123456789abcdef"

def _short_digest(text):
    """A short stable digest of a string, for disambiguating long peer suffixes.

    Starlark's `hash` is Java's String.hashCode, which is fine here: it only has
    to separate the peer sets of one package from each other, and the readable
    prefix it is appended to carries the meaning.
    """
    h = hash(text) & _MASK32
    out = ""
    for i in range(8):
        out = _HEX_DIGITS[(h >> (i * 4)) & 15] + out
    return out

_DIR_SAFE = "abcdefghijklmnopqrstuvwxyz0123456789_-"

def _peer_suffix_dir_name(peer_suffix):
    """A filesystem-safe, stable component naming one peer set, or "" for none.

    Readable prefix plus a digest of the whole suffix: nested peer sets run to
    hundreds of characters, and truncation alone would collide two resolutions
    into one repository -- which is the bug this whole keying exists to avoid.
    """
    if not peer_suffix:
        return ""
    cleaned = ""
    for ch in peer_suffix.lower().elems():
        cleaned += ch if ch in _DIR_SAFE else "_"
    trimmed = ""
    prev_us = False
    for ch in cleaned.elems():
        if ch == "_":
            if not prev_us:
                trimmed += ch
            prev_us = True
        else:
            trimmed += ch
            prev_us = False
    trimmed = trimmed.strip("_")
    if len(trimmed) > 40:
        trimmed = trimmed[:40].strip("_")
    return "{}_{}".format(trimmed, _short_digest(peer_suffix))

def _snapshot_dir_name(snapshot):
    """The directory/repository-name component identifying one resolution.

    A peer-free snapshot keeps the plain `<label>__<version>` name it has always
    had, so the common case produces the same repository names as before.
    """
    base = _package_dir_name(snapshot["name"], snapshot["version"])
    suffix = _peer_suffix_dir_name(snapshot["peer_suffix"])
    return base if not suffix else "{}__{}".format(base, suffix)

# ─── Public surface ────────────────────────────────────────────────────────────

parse_pnpm_lock = _parse_pnpm_lock
parse_importers = _parse_importers
parse_patched_dependencies = _parse_patched_dependencies
npm_tarball_url = _npm_tarball_url
local_tarball_path = _local_tarball_path
verify_integrity = _verify_integrity
npmrc_registries = _npmrc_registries
pnpm_workspace_registries = _pnpm_workspace_registries
workspace_registries = _workspace_registries
package_scope = _package_scope
npmrc_assignments = _npmrc_assignments
package_name_to_label = _package_name_to_label
package_dir_name = _package_dir_name
versioned_label_name = _versioned_label_name
semver_parts = _semver_parts
semver_gt = _semver_gt
pkg_matches_platform = _pkg_matches_platform
snapshot_dir_name = _snapshot_dir_name
peer_suffix_dir_name = _peer_suffix_dir_name
dep_snapshot_id = _dep_snapshot_id
snapshot_parts = _snapshot_parts
