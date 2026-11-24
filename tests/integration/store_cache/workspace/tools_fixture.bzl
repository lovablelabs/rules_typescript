"""A tools_toolchain_repo over the tarball the runner writes: the test fetches
no tools release."""

load("@rules_typescript//ts/private:toolchain.bzl", "tools_toolchain_repo")

def _tools_fixture_impl(module_ctx):
    tools_toolchain_repo(
        name = "tools_fixture",
        url = "file://" + str(module_ctx.path(Label("//:tools.tar.gz"))),
        integrity = module_ctx.read(Label("//:tools.integrity")).strip(),
        strip_prefix = "tools",
    )
    tools_toolchain_repo(
        name = "tools_unpinned",
        url = "file://" + str(module_ctx.path(Label("//:tools.tar.gz"))),
        integrity = "",
        strip_prefix = "tools",
    )

tools_fixture = module_extension(implementation = _tools_fixture_impl)
