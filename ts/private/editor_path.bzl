def editor_project_path(package, name):
    """Retains the emitted compiler config's package-local ambient lookup."""
    return "/".join([part for part in [package, ".bazel/tsconfig", name + ".json"] if part])
