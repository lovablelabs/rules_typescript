"""runtime_scope_destinations against the linear scan over every scope it replaced."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//ts/private:providers.bzl", "runtime_scope_destinations", "runtime_scope_index")

def _linear_destinations(scopes, source, runtime_path):
    source_dir = source.short_path.split("/")[:-1]
    runtime_dir = runtime_path.split("/")[:-1]
    nearest = {}
    depth = -1
    for scope in scopes:
        if source.owner.workspace_root != scope.owner.workspace_root:
            continue
        scope_dir = scope.short_path.split("/")[:-1]
        if source_dir[:len(scope_dir)] != scope_dir:
            continue
        scope_depth = len(scope_dir)
        if scope_depth > depth:
            nearest = {}
            depth = scope_depth
        if scope_depth == depth:
            distance = len(source_dir) - scope_depth
            nearest[scope] = "/".join(runtime_dir[:len(runtime_dir) - distance] + [scope.basename]) if distance <= len(runtime_dir) else None
    return nearest

def _file(short_path, workspace_root = ""):
    return struct(
        short_path = short_path,
        basename = short_path.rpartition("/")[2],
        owner = struct(workspace_root = workspace_root),
    )

def _runtime_scope_destinations_test(ctx):
    env = unittest.begin(ctx)
    scopes = [
        _file("package.json"),
        _file("app/package.json"),
        _file("app/lib/package.json"),
        _file("app/lib/scope.json"),
        _file("app/library/package.json"),
        _file("app/lib/package.json"),
        _file("../dep/app/package.json", "external/dep"),
        _file("../dep/package.json", "external/dep"),
    ]
    sources = [
        _file("root.ts"),
        _file("app/a.ts"),
        _file("app/lib/b.ts"),
        _file("app/lib/deep/c.ts"),
        _file("app/library/d.ts"),
        _file("app/libx/e.ts"),
        _file("app/lib/package.json"),
        _file("../dep/app/f.ts", "external/dep"),
        _file("../dep/g.ts", "external/dep"),
        _file("../other/h.ts", "external/other"),
    ]
    runtimes = ["bin/x.js", "x.js", "bin/out/app/lib/deep/c.js", "a/b/c/d/e/f.js"]
    for subset in [scopes, scopes[1:3], scopes[3:], [scopes[2], scopes[1]], []]:
        index = runtime_scope_index(subset)
        for source in sources:
            for runtime in runtimes:
                asserts.equals(
                    env,
                    _linear_destinations(subset, source, runtime).items(),
                    runtime_scope_destinations(index, source, runtime).items(),
                    "{} at {}".format(source.short_path, runtime),
                )
    return unittest.end(env)

runtime_scope_destinations_test = unittest.make(_runtime_scope_destinations_test)

def runtime_scope_test_suite(name):
    unittest.suite(name, runtime_scope_destinations_test)
