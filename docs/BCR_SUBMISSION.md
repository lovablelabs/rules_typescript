# Bazel Central Registry (BCR) Submission Guide

Current status: nothing has been released. There are no git tags, no GitHub
releases, and `.bcr/metadata.json` still has an empty `versions` list, so
`registry.bazel.build/modules/rules_typescript` does not exist.
`.bcr/source.json` is a placeholder: its URL points at a tarball that was never
published and its `integrity` is empty. The release workflow fills both in when a
tag is pushed. Until then consumers pin the ruleset with a non-registry override,
per
[Depending on rules_typescript](getting-started/quickstart.md#depending-on-rules_typescript).

## Overview

Pushing a tag does everything up to and including the `.bcr/source.json` PR.
Getting that into the registry is a pull request a person opens by hand against
another repository.

1. **Release Workflow** (`.github/workflows/release.yml`): automated
   - Triggered on git tag push (e.g. `git tag v0.2.0`)
   - Builds the `git archive` tarball, computes the SHA256 in hex and SRI form
   - Creates the GitHub release with a build-provenance attestation
   - Opens the PR that fills in `.bcr/source.json`

2. **Publish to BCR Workflow** (`.github/workflows/publish-to-bcr.yml`): a
   pre-flight check. It asserts that `.bcr/metadata.json`, `.bcr/source.json` and
   `.bcr/presubmit.yml` exist and that the two JSON files parse (`jq -e`),
   `HEAD`s the tarball URL (a warning, not a failure), prints the manual
   submission checklist, and uploads the three files as an artifact. On a
   release event it also rewrites that release's notes to point at the metadata.
   It opens no pull request against the registry. The manual route is
   [Submission Steps](#submission-steps) below.

## Release Process

### 1. Create a Release

Release versions follow semantic versioning: `X.Y.Z`

```bash
# Set version variable
VERSION="0.2.0"

# Create and push the tag by hand
git tag -a "v${VERSION}" -m "rules_typescript ${VERSION}"
git push origin "v${VERSION}"
```

Or use the release tool, which does the local half:

```bash
bazel run //tools/release -- 0.2.0 --dry-run   # print every step, mutate nothing
bazel run //tools/release -- 0.2.0 --push      # bump, commit, tag, push
```

It validates the version format, checks that the git working tree is clean, bumps
`module(version)` in `MODULE.bazel`, commits that, creates the annotated tag
`v<version>`, and with `--push` pushes it, which starts the Release workflow. It
builds no tarball and computes no integrity hash: an archive built locally is not
the archive GitHub publishes. Everything downstream of the tag (`git archive`,
the GitHub release, the SRI hash and the `.bcr/source.json` update) is
`.github/workflows/release.yml`.

### 2. GitHub Release Creation

When you push the tag, GitHub Actions automatically:

- Builds the release tarball
- Generates SLSA attestation for supply chain security
- Creates a GitHub Release
- Opens a PR updating `.bcr/source.json`

The release workflow output includes:
- **version**: Semantic version (e.g., 0.2.0)
- **tarball**: Compressed archive (e.g., rules_typescript-0.2.0.tar.gz)
- **sha256**: The hash in hex, as `sha256sum` prints it
- **integrity**: The same hash in SRI format (`sha256-<base64>`), which is what
  `.bcr/source.json` carries

### 3. Verify Release Artifacts

After the workflow completes:

1. Check GitHub Releases: https://github.com/mikn/rules_typescript/releases
2. Verify tarball download works
3. Check SHA256 hash matches the build output

```bash
# Verify integrity
wget https://github.com/mikn/rules_typescript/releases/download/v0.2.0/rules_typescript-0.2.0.tar.gz
sha256sum rules_typescript-0.2.0.tar.gz
```

## BCR Submission Process

### Prerequisites

- Release tag exists on GitHub with tarball and attestation
- All CI checks pass (tests, validation, examples)
- MODULE.bazel version is updated
- .bcr/source.json has correct integrity hash

### Submission Steps

The BCR submission must be done via GitHub PR to https://github.com/bazelbuild/bazel-central-registry

#### Copy the Metadata into a BCR Fork

1. Fork https://github.com/bazelbuild/bazel-central-registry

2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR-USERNAME/bazel-central-registry.git
   cd bazel-central-registry
   ```

3. Create a feature branch:
   ```bash
   git checkout -b rules_typescript-v0.2.0
   ```

4. Create the module directory:
   ```bash
   mkdir -p modules/rules_typescript/0.2.0
   ```

5. Copy metadata files from rules_typescript repo:
   ```bash
   # Get the files
   cp /path/to/rules_typescript/.bcr/metadata.json modules/rules_typescript/
   cp /path/to/rules_typescript/.bcr/source.json modules/rules_typescript/0.2.0/
   cp /path/to/rules_typescript/.bcr/presubmit.yml modules/rules_typescript/0.2.0/
   ```

6. Verify the files:
   ```bash
   cat modules/rules_typescript/metadata.json
   cat modules/rules_typescript/0.2.0/source.json
   cat modules/rules_typescript/0.2.0/presubmit.yml
   ```

7. Commit and push:
   ```bash
   git add modules/rules_typescript/
   git commit -m "Add rules_typescript 0.2.0"
   git push origin rules_typescript-v0.2.0
   ```

8. Create PR on GitHub:
   - Go to https://github.com/bazelbuild/bazel-central-registry
   - Click "New Pull Request"
   - Select your fork and branch
   - Fill PR title: "Add rules_typescript 0.2.0"
   - Fill PR description with details from release notes

#### The Pre-Flight Check

The steps above are the only submission route. This workflow validates what you
are about to copy and prints the same checklist:

```bash
gh workflow run publish-to-bcr.yml \
  -f version=0.2.0 \
  -R mikn/rules_typescript
```

It also lists `release: [published]` as a trigger, but the release
`release.yml` creates does not fire it: GitHub starts no workflow run from an
event raised with the default `GITHUB_TOKEN`. A release published by hand does
fire it. See [CI/CD](CI_CD.md#bcr-bazel-central-registry-publishing).

### BCR Metadata Files

#### .bcr/metadata.json

Contains module-level information (shared across all versions):

```json
{
  "homepage": "https://github.com/mikn/rules_typescript",
  "maintainers": [
    {
      "name": "Your Name",
      "email": "you@example.com",
      "github": "your-github-handle"
    }
  ],
  "repository": ["github:mikn/rules_typescript"],
  "versions": [],
  "yanked_versions": {}
}
```

**Note**: The BCR system maintains the `versions` and `yanked_versions` arrays. Do not edit them by hand.

#### .bcr/source.json

Version-specific release information:

```json
{
  "url": "https://github.com/mikn/rules_typescript/releases/download/v0.2.0/rules_typescript-0.2.0.tar.gz",
  "integrity": "sha256-<base64-hash>",
  "strip_prefix": "rules_typescript-0.2.0"
}
```

Fields:
- **url**: Direct link to release tarball (must be GitHub Releases)
- **integrity**: SRI-format hash (sha256-base64)
- **strip_prefix**: Top-level directory in tarball (removes prefix when extracting)

#### .bcr/presubmit.yml

Testing configuration for BCR's CI system:

```yaml
bcr_test_module:
  module_path: "e2e/basic"
  matrix:
    platform:
      - debian11
      - macos_arm64
    bazel:
      - "8.x"
      - "9.x"
  tasks:
    run_tests:
      # The Gazelle floor suite tests the ruleset itself; GitHub CI runs it.
      build_targets:
        - "//..."
        - "-//:gazelle_proto_test"
        - "-//:gazelle_test"
        - "-//:gazelle_golden_test"
      test_targets:
        - "//..."
        - "-//:gazelle_proto_test"
        - "-//:gazelle_test"
        - "-//:gazelle_golden_test"
      build_flags:
        - "--keep_going"
      test_flags:
        - "--test_output=short"
```

Defines:
- **module_path**: Path to test module within the repository
- **matrix**: Combinations of platforms and Bazel versions to test
- **tasks**: Build and test targets, plus the flags each invocation gets

### Adding SOURCE.md (Optional)

For complex build requirements, add `SOURCE.md` at the repository root, which is
where `publish-to-bcr.yml` looks for it:

```markdown
# Building rules_typescript from source

## Prerequisites
- Bazel 9+
- Rust through rules_rs 0.0.111; C/C++ through hermetic LLVM 0.8.21
- Go 1.26+

## Build Instructions

### Building oxc_cli

rules_rs builds the Oxc CLI from the Cargo.toml and Cargo.lock under `oxc_cli/`.

```

Add this when the standard tarball extraction and build procedure needs documentation.

## CI/CD Workflows

### Release Workflow

**File**: `.github/workflows/release.yml`

**Triggered by**: Git tag push matching `v*`

**Steps**:
1. Extract version from tag
2. Build release tarball from git archive
3. Compute SHA256 hash (both hex and SRI format)
4. Generate SLSA build provenance attestation
5. Create GitHub Release with tarball
6. Upload release info for BCR workflow
7. Rewrite `.bcr/source.json` with the URL, integrity hash and strip_prefix
8. Create PR with the updated .bcr/source.json

**Outputs**:
- GitHub Release with tarball and attestation
- PR updating .bcr/source.json with correct hash

### Publish to BCR Workflow

**File**: `.github/workflows/publish-to-bcr.yml`

**Triggered by**:
- Manual workflow dispatch with version
- Automatic on release publication

**Steps**:
1. Determine version (from workflow input or release tag)
2. Validate BCR metadata files exist
3. Validate JSON format of metadata files
4. Verify release exists on GitHub
5. Check tarball download availability
6. Generate submission summary and checklist
7. On a release event, replace the release notes with a line pointing at the BCR
   metadata (`gh release edit --notes`), which overwrites the generated notes
8. Upload metadata artifacts for reference

## Release Checklist

Before cutting a release:

- [ ] All commits are properly reviewed and merged
- [ ] CI tests pass (unit tests, E2E, examples)
- [ ] Type checking passes (validation)
- [ ] Determinism check passes
- [ ] Integration tests pass
- [ ] README is up-to-date
- [ ] CHANGELOG or release notes prepared
- [ ] MODULE.bazel version is in main branch (can be done by release script)

For the actual release:

- [ ] Tag version using `git tag v0.2.0` or `bazel run //tools/release -- 0.2.0`
- [ ] Push tag: `git push origin v0.2.0`
- [ ] Verify GitHub Actions workflows complete successfully
- [ ] Download tarball and verify integrity
- [ ] Check .bcr/metadata.json is correct
- [ ] Check .bcr/source.json has correct hash
- [ ] Submit to BCR following the steps above

For BCR submission:

- [ ] Fork bazel-central-registry
- [ ] Create feature branch
- [ ] Copy metadata files to correct location
- [ ] Verify file format and contents
- [ ] Commit and push
- [ ] Create PR with descriptive title and body
- [ ] Wait for BCR pre-submission tests to pass
- [ ] Respond to reviewer feedback if any

## Troubleshooting

### Release Workflow Fails

Check the workflow logs:
- Go to Actions tab in GitHub
- Click on the failed "Release" workflow
- Check step output for error details

Common issues:
- **Module.bazel version mismatch**: Ensure release script was run or version was manually updated
- **Tarball creation failed**: Check git archive command and directory structure
- **Hash computation failed**: Verify sha256sum and xxd are available (should be on Ubuntu)

### BCR Metadata Issues

The BCR PR's own presubmit validates all three files and names the file and line
it rejected. It is the authority. For a faster local check on the two JSON files,
with nothing to install:

```bash
python3 -m json.tool .bcr/metadata.json  > /dev/null
python3 -m json.tool .bcr/source.json    > /dev/null
```

`json.tool` is stdlib, so any `python3` will do. `.bcr/presubmit.yml` has no
stdlib equivalent; the presubmit covers that one.

This is a maintainer's local convenience. No rule, action or toolchain in
rules_typescript uses Python.

### Integrity Hash Mismatch

Recalculate the correct hash:

```bash
VERSION="0.2.0"
TARBALL="rules_typescript-${VERSION}.tar.gz"

# Download from GitHub release
wget https://github.com/mikn/rules_typescript/releases/download/v${VERSION}/${TARBALL}

# Calculate SHA256 hash in SRI format
SHA256_HEX=$(sha256sum ${TARBALL} | awk '{print $1}')
SHA256_BASE64=$(echo -n "${SHA256_HEX}" | xxd -r -p | base64 -w0)

echo "sha256-${SHA256_BASE64}"
```

Update .bcr/source.json with the correct integrity value.

## BCR Submission Best Practices

1. **Use GitHub Releases**: The tarball must be hosted on GitHub Releases, not arbitrary URLs
2. **Include attestation**: Always generate SLSA provenance attestation for supply chain security
3. **Test before submission**: Run the e2e/basic tests to ensure module works
4. **Clear descriptions**: Provide detailed release notes and PR descriptions
5. **Respond quickly**: BCR maintainers may request changes or clarifications
6. **Semantic versioning**: Follow SemVer for version numbering (e.g., 0.2.0, not v0.2.0)
7. **Minimal files**: Only include necessary files in tarball, exclude test artifacts and caches

## References

- [Bazel Central Registry Documentation](https://github.com/bazelbuild/bazel-central-registry)
- [Module Metadata Schema](https://github.com/bazelbuild/bazel-central-registry#metadata)
- [Source Configuration](https://github.com/bazelbuild/bazel-central-registry#source-configuration)
- [Presubmit Testing](https://github.com/bazelbuild/bazel-central-registry#testing-your-module)
- [SLSA Framework](https://slsa.dev/)
