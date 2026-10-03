"""Direct npm dependencies keep the resolution selected by the importer."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//tests:runnable_actions.bzl", "RunnableActionOwnerInfo", "runnable_action_aspect", "runnable_actions")
load("//tools/launcher:launcher.bzl", "rlocation_path")
load("//ts:defs.bzl", "TsInfo", "ts_test")
load("//ts/private:providers.bzl", "NodeModulesInfo", "NpmLinkInfo", "NpmPackageInfo")

TEST_RUNFILES_REPOSITORY = Label("//:BUILD.bazel").workspace_name or "_main"

def _direct_npm_identity_impl(ctx):
    env = analysistest.begin(ctx)
    for message in ctx.attr.messages:
        asserts.expect_failure(env, message)
    return analysistest.end(env)

direct_npm_identity_test = analysistest.make(
    _direct_npm_identity_impl,
    expect_failure = True,
    attrs = {"messages": attr.string_list(mandatory = True)},
)

def npm_identity_runtime_test(name, node_modules, deps, expected, src_visibility = None):
    write_file(
        name = name + "_src",
        out = name + ".test.ts",
        content = [
            'import assert from "node:assert/strict";',
            'import { test } from "vitest";',
            'import { greet } from "shared";',
            "",
            'test("shared never resolves to a different store identity", () => {',
            '  assert.equal(greet("World"), ' + json.encode(expected) + ");",
            "});",
            "",
        ],
        visibility = src_visibility,
    )
    ts_test(
        name = name,
        size = "small",
        srcs = [":" + name + "_src"],
        emit = True,
        node_modules = node_modules,
        deps = deps + ["@npm//:vitest"],
    )

def _mixed_npm_store_identity_impl(ctx):
    env = analysistest.begin(ctx)
    selected = ctx.attr.selected[NpmPackageInfo]
    retained = ctx.attr.retained[NpmPackageInfo]
    asserts.equals(env, selected.store.key, retained.store.key, "the countercontrol shares a package key")
    asserts.true(env, selected.store.tree != retained.store.tree, "the stores must be distinct Files")
    if ctx.attr.asset_owner:
        asset_info = ctx.attr.asset_owner[TsInfo]
        owners = asset_info.owners.to_list()
        assets = [pair for owner in owners for pair in owner.asset_files]
        asserts.equals(env, 1, len(assets), "the real producer publishes one executable asset")
        asserts.equals(env, [], [pair for owner in owners for pair in owner.runtime_files], "the asset owner has no runtime_files fallback")
        if assets:
            original, _coordinate, published = assets[0]
            asserts.equals(env, "tests/npm/app_b/helper.mjs", original.short_path)
            asserts.true(env, published in ctx.attr.asset_consumer[TsInfo].transitive_data.to_list(), "the dev entry retains the executable asset")
        bindings = [store for owner in owners for name, _link, store in owner.npm_bindings if name == "shared"]
        asserts.equals(env, [retained.store.tree], bindings, "the asset owner's binding selects the retained exact store File")
    checks = [action for action in runnable_actions(env) if action.mnemonic == "TsgoCheck"]
    asserts.equals(env, 1, len(checks))
    if checks:
        for tree in [selected.store.tree, retained.store.tree]:
            asserts.true(env, tree in checks[0].inputs.to_list(), "both exact stores remain compiler inputs")
    writers = [action for action in runnable_actions(env) if any([file.basename.endswith(".ownership") for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(writers))
    if writers:
        rows = [line.split("\t") for line in writers[0].content.split("\n") if line]
        direct = [row[2] for row in rows if row[0] == "npm-direct"]
        reachable = [row[3] for row in rows if row[0] == "npm"]
        asserts.true(env, selected.store.tree.path in direct, "selected File A is direct")
        asserts.equals(env, ctx.attr.retained_direct, retained.store.tree.path in direct, "only declaring File B makes it direct")
        asserts.equals(env, not ctx.attr.retained_direct, retained.store.tree.path in reachable, "same-key File B retains its closure ownership")
    return analysistest.end(env)

mixed_npm_store_identity_test = analysistest.make(
    _mixed_npm_store_identity_impl,
    attrs = {
        "asset_consumer": attr.label(providers = [TsInfo]),
        "asset_owner": attr.label(providers = [TsInfo]),
        "selected": attr.label(providers = [NpmPackageInfo], mandatory = True),
        "retained": attr.label(providers = [NpmPackageInfo], mandatory = True),
        "retained_direct": attr.bool(),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _dev_npm_path(env, target):
    configs = [action for action in target[RunnableActionOwnerInfo].actions if any([file.basename.endswith("_launcher.json") for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(configs), "one launcher owns the actual npm lookup path")
    return json.decode(configs[0].content)["dev_server"]["node_modules"] if configs else ""

def _inherited_dev_npm_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    importer = ctx.attr.importer[NodeModulesInfo]
    entry = ctx.attr.entry[TsInfo]
    store = ctx.attr.package[NpmPackageInfo].store.tree
    name = ctx.attr.package[NpmPackageInfo].package_name
    parent_link = importer.parent.links[name].link
    view = _dev_npm_path(env, target)
    roots = {link.path: link.target_file for link in target[DefaultInfo].default_runfiles.root_symlinks.to_list()}
    files = target[DefaultInfo].default_runfiles.files.to_list()
    asserts.false(env, name in importer.links, "the child must not redeclare the inherited package")
    bindings = [(link, selected) for owner in entry.owners.to_list() for bound_name, link, selected in owner.npm_bindings if bound_name == name]
    asserts.equals(env, [(parent_link, store)], bindings, "the real compiler retains the parent's exact binding")
    asserts.equals(env, store, roots.get(view + "/" + name), "the configured app lookup reaches the exact parent store")
    asserts.true(env, parent_link in files and store in files, "the original link and store stay declared")
    for direct_name, link in importer.links.items():
        asserts.true(env, link.link in files, "original direct links stay present")
        asserts.equals(env, link.store.tree, roots.get(view + "/" + direct_name), "direct names select their original exact stores")
    if ctx.attr.other:
        other = ctx.attr.other[DefaultInfo].default_runfiles
        other_store = ctx.attr.other_package[NpmPackageInfo].store.tree
        other_name = ctx.attr.other_package[NpmPackageInfo].package_name
        other_view = _dev_npm_path(env, ctx.attr.other)
        asserts.true(env, view != other_view, "two dev targets sharing an importer have separate views")
        asserts.false(env, view.startswith(other_view + "/") or other_view.startswith(view + "/"), "slash-containing labels cannot nest one view below another")
        first_names = {bound_name: True for owner in entry.owners.to_list() for bound_name, _link, _store in owner.npm_bindings}
        other_names = {bound_name: True for owner in ctx.attr.other_entry[TsInfo].owners.to_list() for bound_name, _link, _store in owner.npm_bindings}
        asserts.true(env, name in first_names and name not in other_names, "first entry has its own inherited requirement")
        asserts.true(env, other_name in other_names and other_name not in first_names, "second entry has a disjoint inherited requirement")
        merged_runfiles = [target[DefaultInfo].default_runfiles.merge(other), other.merge(target[DefaultInfo].default_runfiles)]
        if ctx.attr.consumers:
            merged_runfiles = [consumer[DefaultInfo].data_runfiles for consumer in ctx.attr.consumers]
            asserts.equals(env, 2, len(merged_runfiles), "both enclosing data orders are actual Bazel runfiles")
        for merged in merged_runfiles:
            merged_roots = {link.path: link.target_file for link in merged.root_symlinks.to_list()}
            if ctx.file.ordinary_data:
                data_path = rlocation_path(ctx, ctx.file.ordinary_data)
                asserts.true(env, ctx.file.ordinary_data in merged.files.to_list(), "enclosing ordinary data remains declared")
                asserts.true(env, data_path.endswith(view.split("/", 1)[1] + "/" + name), "the ordinary File exercises the same view/package suffix")
                asserts.true(env, data_path.split("/", 1)[0] != view.split("/", 1)[0], "canonical repository Files cannot occupy the private root")
                asserts.false(env, any([data_path == path or data_path.startswith(path + "/") or path.startswith(data_path + "/") for path in merged_roots]), "no private root alias hides the enclosing File")
            for selected_view in [view, other_view]:
                asserts.equals(env, store, merged_roots.get(selected_view + "/" + name), "live edits retain the declared companion in both views")
                asserts.equals(env, other_store, merged_roots.get(selected_view + "/" + other_name), "optimizer lookup retains all declared parent names in both views")
    return analysistest.end(env)

inherited_dev_npm_test = analysistest.make(
    _inherited_dev_npm_impl,
    attrs = {
        "entry": attr.label(providers = [TsInfo], mandatory = True),
        "importer": attr.label(providers = [NodeModulesInfo], mandatory = True),
        "package": attr.label(providers = [NpmPackageInfo], mandatory = True),
        "consumers": attr.label_list(),
        "ordinary_data": attr.label(allow_single_file = True),
        "other": attr.label(aspects = [runnable_action_aspect]),
        "other_entry": attr.label(providers = [TsInfo]),
        "other_package": attr.label(providers = [NpmPackageInfo]),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _member_dev_npm_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    member = ctx.attr.member[NpmLinkInfo]
    parent_member = ctx.attr.parent_member[NpmLinkInfo]
    importer = ctx.attr.importer[NodeModulesInfo]
    name = ctx.attr.member[NpmPackageInfo].package_name
    registry = importer.parent.links[name]
    asserts.true(env, registry.store.tree != member.store.tree, "the parent registry is a distinct store")
    asserts.true(env, member.link != parent_member.link, "the same-store control uses different link Files")
    asserts.equals(env, member.store.tree, parent_member.store.tree, "both member links select the exact same store")
    bindings = [(link, store) for owner in ctx.attr.entry[TsInfo].owners.to_list() for bound_name, link, store in owner.npm_bindings if bound_name == name]
    asserts.equals(env, {member.link: member.store.tree, parent_member.link: member.store.tree}, dict(bindings), "both real compiler contexts retain their original link identities")
    other_bindings = [(link, store) for owner in ctx.attr.other_entry[TsInfo].owners.to_list() for bound_name, link, store in owner.npm_bindings if bound_name == name]
    asserts.equals(env, [(registry.link, registry.store.tree)], other_bindings, "the other entry selects the same importer's parent registry")
    files = target[DefaultInfo].default_runfiles.files.to_list()
    asserts.true(env, member.link in files and parent_member.link in files, "existing coordinate authorities are preserved")
    view = _dev_npm_path(env, target)
    other_view = _dev_npm_path(env, ctx.attr.other)
    asserts.true(env, view != other_view, "different effective bindings never share an app view")
    other = ctx.attr.other[DefaultInfo].default_runfiles
    for merged in [target[DefaultInfo].default_runfiles.merge(other), other.merge(target[DefaultInfo].default_runfiles)]:
        roots = {link.path: link.target_file for link in merged.root_symlinks.to_list()}
        asserts.equals(env, member.store.tree, roots.get(view + "/" + name), "the nearer member wins in its own view")
        asserts.equals(env, registry.store.tree, roots.get(other_view + "/" + name), "the sibling view retains its inherited registry")
    return analysistest.end(env)

member_dev_npm_test = analysistest.make(
    _member_dev_npm_impl,
    attrs = {
        "entry": attr.label(providers = [TsInfo], mandatory = True),
        "importer": attr.label(providers = [NodeModulesInfo], mandatory = True),
        "member": attr.label(providers = [NpmLinkInfo, NpmPackageInfo], mandatory = True),
        "parent_member": attr.label(providers = [NpmLinkInfo], mandatory = True),
        "other": attr.label(aspects = [runnable_action_aspect], mandatory = True),
        "other_entry": attr.label(providers = [TsInfo], mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)

def _previous_dev_npm_owner_impl(ctx):
    info = ctx.attr.dep[TsInfo]
    fields = {field: getattr(info, field) for field in dir(info) if field not in ["to_json", "to_proto"]}
    fields["owners"] = depset([
        struct(**{field: getattr(owner, field) for field in dir(owner) if field not in ["npm_bindings", "to_json", "to_proto"]})
        for owner in info.owners.to_list()
    ])
    return [ctx.attr.dep[DefaultInfo], TsInfo(**fields)]

previous_dev_npm_owner = rule(
    implementation = _previous_dev_npm_owner_impl,
    attrs = {"dep": attr.label(providers = [TsInfo], mandatory = True)},
)

def _previous_dev_npm_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = ctx.attr.entry[TsInfo]
    member = ctx.attr.member[NpmLinkInfo]
    name = ctx.attr.member[NpmPackageInfo].package_name
    importer = ctx.attr.importer[NodeModulesInfo]
    asserts.true(env, member.link in info.npm_files.to_list(), "the prior provider retains its real member File")
    asserts.false(env, any([hasattr(owner, "npm_bindings") for owner in info.owners.to_list()]), "the prior records carry no optional binding metadata")
    asserts.true(env, importer.parent.links[name].store.tree != member.store.tree, "the parent registry cannot stand in for this member")
    view = _dev_npm_path(env, target)
    runfiles = target[DefaultInfo].default_runfiles
    roots = {link.path: link.target_file for link in runfiles.root_symlinks.to_list()}
    alias = roots.get(view + "/" + name)
    asserts.true(env, alias != None and alias.is_symlink, "the view retains the unresolved member through a Bazel-owned alias")
    asserts.true(env, alias in runfiles.files.to_list(), "the runfiles manifest action receives the unresolved alias as an input")
    asserts.true(env, member.link in runfiles.files.to_list(), "the original coordinate still owns the original link File")
    asserts.true(env, member.store.tree in runfiles.files.to_list(), "the member's complete closure stays declared")
    return analysistest.end(env)

previous_dev_npm_test = analysistest.make(
    _previous_dev_npm_impl,
    attrs = {
        "entry": attr.label(providers = [TsInfo], mandatory = True),
        "member": attr.label(providers = [NpmLinkInfo, NpmPackageInfo], mandatory = True),
        "importer": attr.label(providers = [NodeModulesInfo], mandatory = True),
    },
    extra_target_under_test_aspects = [runnable_action_aspect],
)
