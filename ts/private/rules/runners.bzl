"""The two runners ts_test ships, one target each under //ts/runners.

A runner is a target providing TsTestRunnerInfo, the way a toolchain is:
ts_test compiles the tests against the importer chain, and the runner's
`launch` turns them into the launcher config and the runfiles of one test.
vitest runs ES modules whatever the program's module; node:test the package's
format. docs/rules/ts-test.md § Runners.
"""

load("//tools/launcher:launcher.bzl", "rlocation_path", "runfiles_link_path", "runfiles_root_path")
load("//ts/private:providers.bzl", "TsTestRunnerInfo")
load(
    "//ts/private/actions:vitest.bzl",
    "tsconfig_paths_action",
    "vitest_config_action",
)
load("//ts/private/actions:workers_pool.bzl", "workers_pool_environment")

def _vitest_discovery(ctx, test, selected):
    name = getattr(ctx.attr, "public_name", ctx.label.name)
    prefix = "/".join([part for part in [ctx.label.package, "_{}.vitest/tests".format(name)] if part])
    entries = {}
    links = {}
    for source, runtime in test.runtime_inputs.items():
        link = prefix + "/" + rlocation_path(ctx, source)
        path = runfiles_root_path(ctx, link)
        pair = (source, runtime, selected.get(runtime, runtime))
        previous = entries.get(path)
        if previous != None and previous != pair:
            fail("ts_test {}: test discovery path '{}' has conflicting requested inputs or runtimes. Did you mean to give each requested input a distinct discovery path?".format(ctx.label, path))
        entries[path] = pair
        links[link] = pair[2]
    files_list = ctx.actions.declare_file("_{}.vitest/test_files.txt".format(name))
    ctx.actions.write(files_list, "\n".join(sorted(entries)) + "\n")
    return struct(
        root = runfiles_root_path(ctx, prefix),
        entries = entries,
        extensions = sorted({source.extension: True for source in test.runtime_inputs}),
        files_list = files_list,
        symlinks = links,
    )

def _vitest_launch(ctx, test):
    selected = {js: es for js, es in test.es_twins.to_list()}
    pool = workers_pool_environment(ctx, test.chain, test.runtime_data_sets, test.runtime_files, test.asset_files, test.runtime_sources, test.transitive_js)
    selected.update(pool.replacements)
    replacement_links = {runfiles_link_path(original): replacement for original, replacement in selected.items()}
    discovery = _vitest_discovery(ctx, test, selected)
    aliases = {link: selected.get(canonical, canonical) for link, canonical in test.canonical_links}
    program_js = depset([file for file in test.transitive_js.to_list() if file not in selected])
    runtime_data = depset([file for file in depset(transitive = pool.runtime_data_sets).to_list() if file not in aliases and file not in selected])
    package_sources = test.package_sources
    if selected:
        package_sources = [depset([file for file in depset(transitive = package_sources).to_list() if file not in selected])]
    tsconfig_paths = tsconfig_paths_action(ctx)
    written = vitest_config_action(
        ctx,
        test_entry_points = test.entry_points,
        runtime_files = test.runtime_files,
        selected = selected,
        source_sets = package_sources,
        entry_extensions = discovery.extensions,
        discovery_root = discovery.root,
        discovery_entries = discovery.entries,
        tsconfig_paths = tsconfig_paths,
        inline_members = test.inline_members,
        overlays = pool.symlinks | replacement_links,
    )

    # Every path is a runfiles path; the launcher resolves them through the
    # runfiles library, so manifest-only layouts work like symlink trees.
    section = {
        "config_file": written.entry,
        "root_rel": written.root_rel,
        "stage": written.stage,
        "test_files_list": rlocation_path(ctx, discovery.files_list),
        "reads_hook": rlocation_path(ctx, test.runner.hook),
    }
    if test.chain.rlocations:
        section["node_modules"] = test.chain.rlocations

        # The canonical bin entry from vitest's package.json#bin, under the
        # first importer on the chain that links vitest.
        section["vitest_in_tree"] = "vitest/vitest.mjs"

    # A sandboxed test must fail on a stale or missing .snap, never write one,
    # and vitest only stops writing when it believes it is running in CI.
    env = dict(ctx.attr.env)
    env.setdefault("CI", "true")

    files = [written.config, discovery.files_list, test.runner.hook]
    if tsconfig_paths:
        files.append(tsconfig_paths)
    symlinks = pool.symlinks | written.symlinks | replacement_links | {runfiles_link_path(link): target for link, target in aliases.items()}
    for path, file in discovery.symlinks.items():
        if symlinks.setdefault(path, file) != file:
            fail("ts_test {}: private test discovery path '{}' conflicts with another runner input. Did you mean to relocate that input outside the test's private discovery directory?".format(ctx.label, path))
    return struct(
        mode = "vitest",
        section = section,
        env = env,
        files = files,
        symlinks = symlinks,
        replacements = selected,
        transitive_files = depset(transitive = (
            [program_js, runtime_data] + package_sources
        )),
        # The config vitest ran with, for debugging and for the tests that pin
        # the layering.
        output_groups = {"vitest_config": depset([written.config])},
    )

# node:test is configured by CLI flags and the test file alone, so the runner
# refuses the vitest attributes instead of generating a config nothing reads.
def _node_test_launch(ctx, test):
    set_attrs = [
        name
        for name, value in [
            ("config", ctx.attr.config),
            ("config_srcs", ctx.attr.config_srcs),
            ("coverage_provider", ctx.attr.coverage_provider),
            ("wrangler_config", ctx.attr.wrangler_config),
        ]
        if value
    ]
    if set_attrs:
        fail(("ts_test {}: the node:test runner reads none of {}. Every one " +
              "of them configures vitest, which this target does not run. " +
              "Drop them, or drop `runner` to run the test under " +
              "vitest.").format(ctx.label, ", ".join(set_attrs)))

    section = {
        "test_files_list": rlocation_path(ctx, test.test_files_list),
        "resolve_hook": rlocation_path(ctx, test.runner.hook),
    }
    if test.chain.rlocations:
        section["node_modules"] = test.chain.rlocations
    return struct(
        mode = "node_test",
        section = section,
        env = dict(ctx.attr.env),
        files = [test.runner.hook],
        symlinks = {},
        transitive_files = depset(transitive = (
            [test.transitive_js] + test.runtime_data_sets +
            test.package_sources
        )),
        output_groups = {},
    )

def _vitest_runner_impl(ctx):
    return [TsTestRunnerInfo(
        packages = ["vitest"],
        hook = ctx.file._reads_hook,
        es_modules = True,
        supports_source_inputs = True,
        launch = _vitest_launch,
    )]

vitest_runner = rule(
    implementation = _vitest_runner_impl,
    attrs = {
        "_reads_hook": attr.label(
            default = Label("//ts/private:reads_hook.cjs"),
            allow_single_file = True,
        ),
    },
    doc = "The vitest runner: the generated config over the user's, vitest " +
          "from the importer chain's node_modules, the program as ES " +
          "modules whatever its tsconfig's module, and under `bazel run " +
          "<test> -- --reads` the report of the workspace files the tests " +
          "read.",
)

def _node_test_runner_impl(ctx):
    return [TsTestRunnerInfo(
        packages = [],
        hook = ctx.file._resolve_hook,
        es_modules = False,
        supports_source_inputs = False,
        launch = _node_test_launch,
    )]

node_test_runner = rule(
    implementation = _node_test_runner_impl,
    attrs = {
        "_resolve_hook": attr.label(
            default = Label("//ts/private:node_test_hook.mjs"),
            allow_single_file = True,
        ),
    },
    doc = "node's own runner: `node --test` over the compiled files, in the " +
          "module format their tsconfig gives them, with the resolve hook " +
          "that answers a relative `.ts` specifier with the compiled sibling.",
)
