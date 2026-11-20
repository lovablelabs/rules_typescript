"""Read credentials only at fetch time: module-extension results and repository
attributes are serialized into the committed MODULE.bazel.lock."""

load("//npm/private:npm_translate_lock.bzl", "package_scope")

_SETTING = "PNPM_CONFIG__AUTH"

def _expand_env(value, getenv):
    """Substitutes ${VAR} from the fetch environment, the way npm does.

    rctx.getenv registers the variable, so changing it refetches.
    """
    out = ""
    rest = value
    for _ in range(16):
        start = rest.find("${")
        if start == -1:
            break
        end = rest.find("}", start)
        if end == -1:
            break
        out += rest[:start] + getenv(rest[start + 2:end], "")
        rest = rest[end + 1:]
    return out + rest

def _auth_scope(url):
    """The `//host/path/` form npm keys credentials by, for a URL or an .npmrc
    key."""
    for scheme in ("https://", "http://"):
        if url.startswith(scheme):
            url = url[len(scheme):]
            break
    if not url.startswith("//"):
        url = "//" + url
    return url if url.endswith("/") else url + "/"

def npmrc_auth_fields(content, url):
    """The credential fields of the .npmrc scope that best matches a URL.

    npm keys credentials by `//host/path/` and applies the LONGEST matching
    prefix, so a registry mounted on a path (an Artifactory repo, say) carries
    its own token without claiming the whole host.

    Args:
        content: The .npmrc text.
        url: The tarball URL about to be fetched.

    Returns:
        (scope, {field: raw_value}) -- raw because ${VAR} is expanded by the
        caller, which is the only place the environment is available.
    """
    request_scope = _auth_scope(url)
    best_scope = ""
    fields = {}
    for raw in content.split("\n"):
        line = raw.strip()
        if not line or line[0] in "#;[":
            continue
        key, sep, value = line.partition("=")
        if not sep or not key.strip().startswith("//"):
            continue
        scope, _, field = key.strip().rpartition(":")
        scope = _auth_scope(scope)
        if not request_scope.startswith(scope) or len(scope) < len(best_scope):
            continue
        if scope != best_scope:
            best_scope = scope
            fields = {}
        fields[field] = value.strip().strip("'\"")
    return (best_scope, fields)

def _header(kind, secret):
    return {
        "type": "pattern",
        "pattern": kind + " <password>",
        "login": "",
        "password": secret,
    }

def _setting_error(what):
    fail(
        "npm: {} {}. pnpm's `auth` setting is a JSON ".format(_SETTING, what) +
        "object keyed by registry URL, then by scope (\"@\" for every " +
        "package there, or \"@org\"), with `authToken` its one field: " +
        "{\"https://npm.example.com\": {\"@acme\": {\"authToken\": \"...\"}}}",
    )

def _is_scope(scope):
    if scope == "@":
        return True
    if len(scope) < 2 or scope[0] != "@":
        return False
    return "/" not in scope and ":" not in scope

def _setting_tokens(getenv):
    """{"//host/path/": {"@": token, "@scope": token}} from PNPM_CONFIG__AUTH,
    read under pnpm's two spellings, the lower-case one first."""
    raw = getenv("pnpm_config__auth", "") or getenv(_SETTING, "")
    if not raw:
        return {}
    parsed = json.decode(raw, default = None)
    if type(parsed) != "dict":
        _setting_error("is not a JSON object")
    tokens = {}
    for url, scopes in parsed.items():
        if type(scopes) != "dict":
            _setting_error("[{}] is not an object keyed by scope".format(url))
        for scope, creds in scopes.items():
            where = "[{}][{}]".format(url, scope)
            if not _is_scope(scope):
                _setting_error(where + ": neither \"@\" nor a package scope")
            if type(creds) != "dict" or creds.keys() != ["authToken"]:
                _setting_error(where + ": `authToken` is the one field")
            if type(creds["authToken"]) != "string":
                _setting_error(where + ": `authToken` is not a string")
            tokens.setdefault(_auth_scope(url), {})[scope] = creds["authToken"]
    return tokens

def _longest(tokens, request, scope):
    """(nerf, token) of the longest registry prefixing `request` with an entry
    for `scope`, or ("", "")."""
    best = ("", "")
    for nerf, entries in tokens.items():
        if not request.startswith(nerf) or scope not in entries:
            continue
        if len(nerf) > len(best[0]):
            best = (nerf, entries[scope])
    return best

def credential_getenv(getenv):
    """getenv, and the list of the variables it found set.

    Bazel records each variable a fetch read, with its value, beside the
    repository's repo contents cache entry: a repository that read a
    credential must return repo_metadata(reproducible = False).

    Args:
        getenv: rctx.getenv.

    Returns:
        (getenv, found): found lists each variable getenv returned non-empty.
    """
    found = []

    def read(name, default = None):
        value = getenv(name, default)
        if value:
            found.append(name)
        return value

    return read, found

def auth_for_fetch(rctx, url):
    """The `auth` for rctx.download of url, and whether the fetch may be shared.

    A repository that read a set variable, which Bazel records beside its repo
    contents cache entry, or that sent any credential, literal .npmrc ones
    included, holds what the credential guards: it is not reproducible.

    Args:
        rctx: The repository_ctx of a rule with `npmrc` and `package` attrs.
        url: The tarball URL about to be fetched.

    Returns:
        (auth, reproducible): auth for rctx.download's `auth`.
    """
    npmrc = ""
    if rctx.attr.npmrc:
        # An attr.label is not by itself an input to the fetch: unwatched, a
        # token rotated in .npmrc would leave the fetched repository in place.
        rctx.watch(rctx.attr.npmrc)
        npmrc = rctx.read(rctx.attr.npmrc)
    getenv, found = credential_getenv(rctx.getenv)
    auth = fetch_auth(npmrc, url, rctx.attr.package, getenv, str(rctx.attr.npmrc))
    return auth, not (auth or found)

def fetch_auth(npmrc, url, package, getenv, npmrc_label = ".npmrc"):
    """The `auth` dict rctx.download needs for one tarball URL, or {}.

    pnpm's choice: the `auth` setting's entry for the package's scope on the
    longest registry prefixing the URL; else the longest-prefix unscoped
    credential, the setting's "@" entry over the .npmrc's on one registry.

    Args:
        npmrc: The .npmrc text, or "" without one.
        url: The tarball URL about to be fetched.
        package: The npm package the URL holds; its scope picks the entry.
        getenv: rctx.getenv, which registers each variable it reads.
        npmrc_label: How the .npmrc is named in an error.

    Returns:
        {url: auth_spec} for rctx.download's `auth`, or {} when neither source
        grants the URL anything.
    """
    request = _auth_scope(url)
    tokens = _setting_tokens(getenv)
    scope = package_scope(package)
    if scope:
        _, token = _longest(tokens, request, scope)
        if token:
            return {url: _header("Bearer", token)}
    nerf, token = _longest(tokens, request, "@")
    npmrc_scope, fields = npmrc_auth_fields(npmrc, url)
    if token and len(nerf) >= len(npmrc_scope):
        return {url: _header("Bearer", token)}

    bearer = _expand_env(fields.get("_authToken", ""), getenv)
    if bearer:
        return {url: _header("Bearer", bearer)}

    basic = _expand_env(fields.get("_auth", ""), getenv)
    if basic:
        return {url: _header("Basic", basic)}

    if "_password" in fields:
        fail(
            "npm: {} configures `username`/`_password` for {}, ".format(
                npmrc_label,
                npmrc_scope,
            ) +
            "which this ruleset cannot use: npm stores `_password` " +
            "base64-encoded and Starlark has no way to decode it (there is " +
            "no chr()). Use `_authToken=` or `_auth=` instead -- `npm config " +
            "set //host/:_authToken` writes the first, and `_auth` is the " +
            "same base64 blob you already have.",
        )
    if token:
        return {url: _header("Bearer", token)}
    return {}
