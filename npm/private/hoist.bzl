"""pnpm's hidden hoist, computed over the lockfile graph.

The rule is pnpm's own hoist step (installing/linking/hoist in pnpm 11.5.3,
identical in 10.32.1): docs/rules/node-modules.md § The Store states it.
tests/npm/hoisted_dependencies.bzl holds pnpm 12.16.1's answer over the same
graph.
"""

load("//npm/private:npm_translate_lock.bzl", "npmrc_assignments", "pnpm_workspace_hoist_settings")

_PATTERN_KEYS = ("hoist-pattern", "public-hoist-pattern")
_FLAG_KEYS = ("hoist", "hoist-workspace-packages")

def hoist_settings(npmrc, pnpm_workspace = None):
    """Effective workspace hoist settings, using pnpm defaults where absent.

    Args:
        npmrc: The lockfile package's .npmrc as text, or None without one.
        pnpm_workspace: Its pnpm-workspace.yaml text, or None without one.

    Returns:
        struct(private, public, workspace_packages): the two pattern lists and
        whether workspace members are hoisted.
    """
    patterns = {key: None for key in _PATTERN_KEYS}
    flags = {key: True for key in _FLAG_KEYS}
    for key, value in npmrc_assignments(npmrc or ""):
        if key.endswith("[]") and key[:-2] in patterns:
            patterns[key[:-2]] = (patterns[key[:-2]] or []) + [value]
        elif key in patterns:
            patterns[key] = [value] if value else []
        elif key in flags:
            flags[key] = value != "false"
    for key, value in pnpm_workspace_hoist_settings(pnpm_workspace).items():
        if key in patterns:
            patterns[key] = value
        else:
            flags[key] = value
    private = patterns["hoist-pattern"]
    if private == None:
        private = ["*"]
    return struct(
        private = private if flags["hoist"] else [],
        public = patterns["public-hoist-pattern"] or [],
        workspace_packages = flags["hoist-workspace-packages"],
    )

def _glob(pattern, name):
    if pattern == "*":
        return True
    parts = pattern.split("*")
    if len(parts) == 1:
        return name == pattern
    if not name.startswith(parts[0]):
        return False
    pos = len(parts[0])
    for part in parts[1:-1]:
        at = name.find(part, pos)
        if at == -1:
            return False
        pos = at + len(part)
    tail = parts[-1]
    return name.endswith(tail) and len(name) - len(tail) >= pos

def matches(patterns, name):
    """pnpm's matcher: `*` is any run, `!` negates, the last matching pattern
    decides, and a list of negations alone matches every name none excludes."""
    includes = [p for p in patterns if not p.startswith("!")]
    if not includes:
        excluded = [_glob(p[1:], name) for p in patterns]
        return len(patterns) > 0 and not any(excluded)
    matched = False
    for pattern in patterns:
        if pattern.startswith("!"):
            if _glob(pattern[1:], name):
                matched = False
        elif not matched and _glob(pattern, name):
            matched = True
    return matched

def _kind(settings, alias):
    if matches(settings.public, alias):
        return "public"
    if matches(settings.private, alias):
        return "private"
    return None

def _walk(lock, start, skipped):
    """(depth, sid) per snapshot reached, pnpm's walk: a level's children are
    marked in order before the first child's subtree is walked."""
    visited = {}
    out = []

    def step(sids):
        nodes = []
        for sid in sids:
            if sid in visited:
                continue
            visited[sid] = True
            if sid in lock.snapshots and sid not in skipped:
                nodes.append(sid)
        return nodes

    stack = [(0, step(start))]
    for _ in range(len(lock.snapshots) + 2):
        if not stack:
            break
        depth, nodes = stack.pop()
        if not nodes:
            continue
        out.extend([(depth, sid) for sid in nodes])
        children = [
            step([child for (_, child) in lock.snapshots[sid].children])
            for sid in nodes
        ]
        stack.extend(reversed([(depth + 1, c) for c in children if c]))
    return out

def hoisted(lock, settings, platform):
    """{alias: (kind, target)}: the link pnpm's hoist step makes per name on
    `platform`, kind "private" or "public", target ("snapshot", sid) or
    ("member", path).

    Args:
        lock: struct(snapshots = {sid: struct(children = [(alias, sid)],
            platforms = [...])}, importers = [(path, [(alias, sid)],
            [(alias, member path)])] in lockfile order, members = [(name,
            path)]).
        settings: hoist_settings().
        platform: The PLATFORMS key whose skipped set applies.
    """
    skipped = {
        sid: True
        for sid, snap in lock.snapshots.items()
        if platform not in snap.platforms
    }
    root = {}
    if settings.workspace_packages:
        for name, path in lock.members:
            root[name] = ("member", path)
    direct = {}
    seed = {}
    start = []
    for path, deps, links in lock.importers:
        for alias, sid in deps:
            if path == ".":
                seed[alias.lower()] = True
            if sid not in lock.snapshots:
                continue
            start.append(sid)
            if alias not in direct:
                direct[alias] = ("snapshot", sid)
        for alias, _ in links:
            if path == ".":
                seed[alias.lower()] = True
    root.update(direct)

    out = {}
    claimed = dict(seed)
    ordered = [(-1, None)] + sorted(_walk(lock, start, skipped))
    for _, sid in ordered:
        if sid == None:
            children = root.items()
        else:
            children = [
                (alias, ("snapshot", child))
                for (alias, child) in lock.snapshots[sid].children
            ]
        for alias, target in children:
            kind = _kind(settings, alias)
            if not kind or alias.lower() in claimed:
                continue
            if target[0] == "snapshot" and (
                target[1] not in lock.snapshots or target[1] in skipped
            ):
                continue
            claimed[alias.lower()] = True
            out[alias] = (kind, target)
    return out
