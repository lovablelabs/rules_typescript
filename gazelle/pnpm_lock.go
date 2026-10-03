package typescript

import (
	"path"
	"strings"
)

const pnpmLockfileName = "pnpm-lock.yaml"

// parsePnpmLockNames is every package name the lockfile mentions -- the keys of
// `packages:` and `snapshots:`, every `link:` and every alias -- the hub gate.
func parsePnpmLockNames(lines []string) map[string]bool {
	names := make(map[string]bool)
	for _, section := range []string{"packages", "snapshots"} {
		body, ok := pnpmSection(lines, section)
		if !ok {
			continue
		}
		for _, raw := range body {
			indent, stripped, ok := pnpmContentLine(raw)
			if !ok || indent != 2 {
				continue
			}
			key := pnpmMappingKey(stripped)
			if key == "" {
				continue
			}
			if name, version := parsePnpmPackageKey(key); name != "" && version != "" {
				names[name] = true
			}
		}
	}
	links, aliases := pnpmLinksAndAliases(parsePnpmImporters(lines))
	for _, name := range links {
		names[name] = true
	}
	for name := range aliases {
		names[name] = true
	}
	return names
}

// pnpmMappingKey returns the key of a mapping line, whichever way its value is
// written: `foo@1.0.0:` opens a block, `foo@1.0.0: {}` puts the mapping on the
// same line. It returns "" for a line that is not a mapping at all.
func pnpmMappingKey(stripped string) string {
	if strings.HasSuffix(stripped, ":") {
		return strings.TrimSuffix(stripped, ":")
	}
	// A YAML key ends at ": "; a `file:` key carries its own colon.
	before, _, found := strings.Cut(stripped, ": ")
	if !found {
		return ""
	}
	return before
}

// parsePnpmImporters reads the `importers:` section: per importer directory
// ("" is the root), the names it declares and the members it links.
func parsePnpmImporters(lines []string) map[string]*pnpmImporter {
	body, ok := pnpmSection(lines, "importers")
	if !ok {
		return nil
	}
	importers := map[string]*pnpmImporter{}
	var current *pnpmImporter
	dir, section, depName := "", "", ""
	record := func(name, value string) {
		value = strings.Trim(strings.TrimSpace(value), "'\"")
		if name == "" || value == "" || strings.HasPrefix(value, "file:") && !pnpmFileTarball(value) {
			return
		}
		if target, isLink := strings.CutPrefix(value, "link:"); isLink {
			// A link: is written relative to the importer that declares it.
			to := path.Clean(path.Join(dir, target))
			if to != "." && !strings.HasPrefix(to, "..") {
				current.links[name] = to
			}
			return
		}
		current.deps[name] = value
	}
	for _, raw := range body {
		indent, stripped, ok := pnpmContentLine(raw)
		if !ok {
			continue
		}
		switch {
		case indent == 2:
			key := pnpmMappingKey(stripped)
			if key == "" {
				continue
			}
			dir = strings.Trim(key, "'\"")
			if dir == "." {
				dir = ""
			}
			current = &pnpmImporter{
				deps: map[string]string{}, links: map[string]string{},
			}
			importers[dir] = current
			section, depName = "", ""
		case current == nil:
		case indent == 4 && strings.HasSuffix(stripped, ":") &&
			!strings.Contains(stripped[:len(stripped)-1], ":"):
			section, depName = strings.TrimSuffix(stripped, ":"), ""
		case section != "dependencies" && section != "devDependencies" &&
			section != "optionalDependencies":
		case indent == 6:
			depName = ""
			// v6 puts the whole entry on the dep's line:
			//   shared: {specifier: workspace:*, version: link:packages/shared}
			if name, rest, found := strings.Cut(stripped, ":"); found && strings.Contains(rest, "{") {
				if _, after, ok := strings.Cut(rest, "version:"); ok {
					value, _, _ := strings.Cut(after, "}")
					value, _, _ = strings.Cut(value, ",")
					record(strings.Trim(strings.TrimSpace(name), "'\""), value)
				}
			} else if strings.HasSuffix(stripped, ":") {
				depName = strings.Trim(strings.TrimSuffix(stripped, ":"), "'\"")
			}
		case indent == 8 && depName != "":
			if key, value, found := strings.Cut(stripped, ":"); found && strings.TrimSpace(key) == "version" {
				record(depName, value)
			}
		}
	}
	return importers
}

// pnpmLinksAndAliases folds the importers into the two name sets the gate
// reads: the link: names, and each npm alias with its target.
func pnpmLinksAndAliases(importers map[string]*pnpmImporter,
) (links []string, aliases map[string]string) {
	aliases = make(map[string]string)
	for _, imp := range importers {
		for name := range imp.links {
			links = append(links, name)
		}
		for name, version := range imp.deps {
			if target := pnpmAliasTarget(version); target != "" {
				aliases[name] = target
			}
		}
	}
	return links, aliases
}

// pnpmSection returns the lines under an indent-0 `<name>:` header, stopping at
// the next indent-0 line.
//
// Anchoring is load-bearing, for the reason npm_translate_lock.bzl gives:
// `catalogs:` is the importers idiom one level shallower and `overrides:`
// entries are shaped like package keys, so a reader that scanned the file for
// `specifier:`/`version:` pairs or for `name@version` keys would pull both in.
func pnpmSection(lines []string, name string) ([]string, bool) {
	header := name + ":"
	for i, line := range lines {
		if strings.TrimRight(line, " \t\r") != header {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if indent, _, ok := pnpmContentLine(lines[j]); ok && indent == 0 {
				return lines[i+1 : j], true
			}
		}
		return lines[i+1:], true
	}
	return nil, false
}

// pnpmContentLine reports a line's indent and its trimmed text, and false for a
// line that carries neither (blank or comment).
func pnpmContentLine(raw string) (indent int, stripped string, ok bool) {
	line := strings.TrimRight(raw, " \t\r")
	stripped = strings.TrimSpace(line)
	if stripped == "" || strings.HasPrefix(stripped, "#") {
		return 0, "", false
	}
	return len(line) - len(strings.TrimLeft(line, " ")), stripped, true
}

// pnpmAliasTarget returns the `name@version` an importer's resolved version
// names, and "" for a plain version. A resolved version is digits and dots;
// only an alias carries the package's own name, and index 0 is skipped so a
// scope's '@' is not the one we find.
func pnpmAliasTarget(version string) string {
	if strings.HasPrefix(version, "file:") {
		return ""
	}
	if paren := strings.Index(version, "("); paren != -1 {
		version = version[:paren]
	}
	if len(version) < 2 || strings.Index(version[1:], "@") == -1 {
		return ""
	}
	name, resolved := parsePnpmPackageKey(version)
	if name == "" || resolved == "" {
		return ""
	}
	return name + "@" + resolved
}

// parsePnpmPackageKey splits a lockfile key into (name, version), and returns
// ("", "") for a key that is neither. Ported from _parse_package_key in
// npm/private/npm_translate_lock.bzl: v6 spells the key `/name@version`, v9
// `name@version`, scoped names arrive single-quoted, and a v9 snapshots key
// carries a peer suffix.
func parsePnpmPackageKey(key string) (name, version string) {
	key = strings.TrimSpace(key)
	if len(key) >= 2 && strings.HasPrefix(key, "'") && strings.HasSuffix(key, "'") {
		key = key[1 : len(key)-1]
	}
	key = strings.TrimPrefix(key, "/")
	if paren := strings.Index(key, "("); paren != -1 {
		key = key[:paren]
	}
	if strings.HasPrefix(key, "@") {
		slash := strings.Index(key, "/")
		if slash == -1 {
			return "", ""
		}
		rest := key[slash+1:]
		at := strings.LastIndex(rest, "@")
		if at <= 0 {
			return "", ""
		}
		return key[:slash+1+at], rest[at+1:]
	}
	at := strings.LastIndex(key, "@")
	if at <= 0 || at == len(key)-1 {
		return "", ""
	}
	return key[:at], key[at+1:]
}

// pnpmFileTarball reports a `file:` archive, which the npm extension fetches
// like a registry package; a `file:` directory has no package to fetch.
func pnpmFileTarball(version string) bool {
	return strings.HasPrefix(version, "file:") &&
		(strings.HasSuffix(version, ".tgz") || strings.HasSuffix(version, ".tar.gz"))
}
