package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
)

func runManifest(args []string) error {
	flags := flag.NewFlagSet("manifest", flag.ExitOnError)
	tsx := flags.String("tsx", ".js",
		"the extension a .tsx emits: .js, or .jsx under jsx: preserve")
	runtimeTargets := flags.String("runtime_targets", "", "declared source-to-runtime package targets as JSON; preserve the authored package format")
	declarationTargets := flags.String("declaration_targets", "", "exact source-to-declaration package targets as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("manifest takes SRC and OUT")
	}
	data, err := os.ReadFile(flags.Arg(0))
	if err != nil {
		return err
	}
	var out []byte
	if *runtimeTargets == "" {
		out, err = manifestAsBuilt(data, *tsx)
	} else {
		var targets []runtimeTarget
		if err := json.Unmarshal([]byte(*runtimeTargets), &targets); err != nil {
			return err
		}
		var declarations []runtimeTarget
		if *declarationTargets != "" {
			if err := json.Unmarshal([]byte(*declarationTargets), &declarations); err != nil {
				return err
			}
		}
		publication := *declarationTargets != ""
		out, err = walkManifest(data, publication, func(target, role, pointer string, key bool) (string, error) {
			selected := targets
			if role != "js" {
				if role != "dts" || !publication {
					return target, nil
				}
				selected = declarations
			}
			projected, constrained, err := projectManifestTarget(target, pointer, selected)
			if err != nil || constrained || !publication || key {
				return projected, err
			}
			return emittedFile(target, role, pointer, *tsx)
		}, nil)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", flags.Arg(0), err)
	}
	return os.WriteFile(flags.Arg(1), append(out, '\n'), 0o644)
}

// The field a target sits under decides what the compile emits for it.
var roleOfField = map[string]string{
	"main":    "js",
	"module":  "js",
	"browser": "js",
	"exports": "js",
	"imports": "js",
	"types":   "dts",
	"typings": "dts",
}

var declarationSuffixes = []string{".d.ts", ".d.mts", ".d.cts"}

type manifestTargetPath struct {
	parts  []string
	suffix string
	url    bool
}

func parseManifestTarget(target, pointer string) (manifestTargetPath, bool, error) {
	isURL := pointer == "/exports" || strings.HasPrefix(pointer, "/exports/") || pointer == "/imports" || strings.HasPrefix(pointer, "/imports/")
	if isURL && !strings.HasPrefix(target, "./") {
		return manifestTargetPath{}, false, nil
	}
	pathname, suffix := target, ""
	if isURL {
		if at := strings.IndexAny(target, "?#"); at >= 0 {
			pathname, suffix = target[:at], target[at:]
		}
		lower := strings.ToLower(pathname)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return manifestTargetPath{}, false, fmt.Errorf("package target %q has an encoded path separator", target)
		}
	}
	parts := strings.Split(pathname, "*")
	if isURL {
		for i, part := range parts {
			decoded, err := url.PathUnescape(part)
			if err != nil {
				return manifestTargetPath{}, false, err
			}
			parts[i] = decoded
		}
	}
	return manifestTargetPath{parts: parts, suffix: suffix, url: isURL}, true, nil
}

func (p manifestTargetPath) identity() manifestTargetPath {
	if p.url {
		return p
	}
	pathname := path.Clean(strings.Join(p.parts, "*"))
	if !path.IsAbs(pathname) {
		pathname = "./" + pathname
	}
	p.parts = []string{pathname}
	return p
}

func (p manifestTargetPath) withParts(parts []string) string {
	if !p.url {
		return strings.Join(parts, "*")
	}
	escaped := make([]string, len(parts))
	for i, part := range parts {
		u := url.URL{Path: part}
		escaped[i] = strings.ReplaceAll(u.EscapedPath(), "*", "%2A")
	}
	return strings.Join(escaped, "*") + p.suffix
}

func emittedFile(target, role, pointer, tsx string) (string, error) {
	var by map[string]string
	switch role {
	case "js":
		by = map[string]string{".tsx": tsx, ".ts": ".js", ".mts": ".mjs",
			".cts": ".cjs"}
	case "dts":
		by = map[string]string{".tsx": ".d.ts", ".ts": ".d.ts",
			".mts": ".d.mts", ".cts": ".d.cts"}
	default:
		return target, nil
	}
	parsed, local, err := parseManifestTarget(target, pointer)
	if err != nil || !local {
		return target, err
	}
	last := len(parsed.parts) - 1
	for _, suffix := range declarationSuffixes {
		if strings.HasSuffix(parsed.parts[last], suffix) {
			return target, nil
		}
	}
	for source, emitted := range by {
		if strings.HasSuffix(parsed.parts[last], source) {
			parsed.parts[last] = strings.TrimSuffix(parsed.parts[last], source) + emitted
			return parsed.withParts(parsed.parts), nil
		}
	}
	return target, nil
}

type runtimeTarget struct {
	Source  string `json:"source"`
	Runtime string `json:"runtime"`
}

type runtimeScopeCheck struct {
	Source  string          `json:"source"`
	Runtime string          `json:"runtime"`
	Targets []runtimeTarget `json:"targets"`
}

func runtimeManifestTarget(target, role, pointer string, targets []runtimeTarget) (string, bool, error) {
	if role != "js" {
		return target, false, nil
	}
	return projectManifestTarget(target, pointer, targets)
}

func projectManifestTarget(target, pointer string, targets []runtimeTarget) (string, bool, error) {
	bare := (pointer == "/main" || pointer == "/module" || pointer == "/browser" || strings.HasPrefix(pointer, "/browser/")) && !strings.HasPrefix(target, ".") && !path.IsAbs(target)
	if bare {
		target = "./" + target
	}
	parsed, local, err := parseManifestTarget(target, pointer)
	if err != nil || !local {
		return target, false, err
	}
	parsed = parsed.identity()
	projected, constrained := target, false
	for _, pair := range targets {
		candidateParts := []string{pair.Runtime}
		count := len(parsed.parts) - 1
		if count == 0 {
			if parsed.parts[0] != pair.Source {
				continue
			}
		} else {
			size := len(pair.Source) - len(strings.Join(parsed.parts, ""))
			prefix := len(parsed.parts[0])
			if size < 0 || size%count != 0 || prefix+size/count > len(pair.Source) {
				continue
			}
			capture := pair.Source[prefix : prefix+size/count]
			if strings.Join(parsed.parts, capture) != pair.Source {
				continue
			}
			candidateParts = append([]string(nil), parsed.parts...)
			if pair.Source != pair.Runtime {
				sourceExtension, runtimeExtension := path.Ext(pair.Source), path.Ext(pair.Runtime)
				for _, suffix := range declarationSuffixes {
					if strings.HasSuffix(pair.Source, suffix) {
						sourceExtension = suffix
					}
					if strings.HasSuffix(pair.Runtime, suffix) {
						runtimeExtension = suffix
					}
				}
				sourceStem := strings.TrimPrefix(strings.TrimSuffix(pair.Source, sourceExtension), "./")
				runtimeStem := strings.TrimSuffix(pair.Runtime, runtimeExtension)
				if prefix, ok := strings.CutSuffix(runtimeStem, sourceStem); ok {
					candidateParts[0] = prefix + strings.TrimPrefix(candidateParts[0], "./")
				}
				candidateParts[count] = strings.TrimSuffix(candidateParts[count], sourceExtension) + runtimeExtension
			}
			if strings.Join(candidateParts, capture) != pair.Runtime {
				return "", false, fmt.Errorf("package target %q cannot represent runtime module %q; use explicit targets", target, pair.Runtime)
			}
		}
		candidate := target
		if pair.Source != pair.Runtime {
			candidate = parsed.withParts(candidateParts)
		}
		if constrained && candidate != projected {
			return "", false, fmt.Errorf("package target %q needs incompatible runtime targets %q and %q; use explicit targets or a compatible scope owner", target, projected, candidate)
		}
		projected, constrained = candidate, true
	}
	if bare {
		projected = strings.TrimPrefix(projected, "./")
	}
	return projected, constrained, nil
}

func validateRuntimeScopes(checks []string) error {
	for _, encoded := range checks {
		var check runtimeScopeCheck
		if err := json.Unmarshal([]byte(encoded), &check); err != nil {
			return err
		}
		if err := validateRuntimeScope(check); err != nil {
			return fmt.Errorf("runtime package scope %s from %s: %w", check.Runtime, check.Source, err)
		}
	}
	return nil
}

func validateRuntimeScope(check runtimeScopeCheck) error {
	source, err := os.ReadFile(check.Source)
	if err != nil {
		return err
	}
	runtime, err := os.ReadFile(check.Runtime)
	if err != nil {
		return err
	}
	expected := map[string]string{}
	expectedStructure := runtimeManifestStructure{}
	_, err = walkManifest(source, false, func(target, role, pointer string, key bool) (string, error) {
		projected, constrained, err := runtimeManifestTarget(target, role, pointer, check.Targets)
		if key {
			return projected, err
		}
		if constrained {
			expected[pointer] = projected
		}
		return target, err
	}, expectedStructure.visit)
	if err != nil {
		return err
	}
	placedStructure := runtimeManifestStructure{}
	_, err = walkManifest(runtime, false, func(target, _, pointer string, key bool) (string, error) {
		if key {
			return target, nil
		}
		if want, ok := expected[pointer]; ok {
			actualPath, actualLocal, actualErr := parseManifestTarget(target, pointer)
			wantedPath, wantedLocal, wantedErr := parseManifestTarget(want, pointer)
			if actualErr != nil || wantedErr != nil || actualLocal != wantedLocal || !reflect.DeepEqual(actualPath.identity(), wantedPath.identity()) {
				return "", fmt.Errorf("target %s is %q, needs %q; publish the scope with all referenced modules in one compatible owner", pointer, target, want)
			}
			delete(expected, pointer)
		}
		return target, nil
	}, placedStructure.visit)
	if err != nil {
		return err
	}
	if len(expected) != 0 {
		return fmt.Errorf("runtime manifest omits declared module targets: %v", expected)
	}
	if !reflect.DeepEqual(expectedStructure, placedStructure) {
		return errors.New("runtime manifest changes runtime resolution structure")
	}
	var original, placed map[string]any
	if err := json.Unmarshal(source, &original); err != nil {
		return err
	}
	if err := json.Unmarshal(runtime, &placed); err != nil {
		return err
	}
	for _, field := range []string{"type", "name"} {
		if !reflect.DeepEqual(original[field], placed[field]) {
			return fmt.Errorf("runtime manifest changes authored %s", field)
		}
	}
	return nil
}

type runtimeManifestStructure map[string]any

func (s runtimeManifestStructure) visit(token json.Token, pointer string, key bool) {
	field := strings.SplitN(strings.TrimPrefix(pointer, "/"), "/", 2)[0]
	if roleOfField[field] != "js" {
		return
	}
	if key {
		s[pointer] = append(s[pointer].([]string), token.(string))
		return
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			s[pointer] = []string{}
		case '}':
			keys := s[pointer].([]string)
			conditional := pointer == "/exports" || strings.HasPrefix(pointer, "/exports/") || strings.HasPrefix(pointer, "/imports/")
			if pointer == "/exports" {
				for _, key := range keys {
					if strings.HasPrefix(key, ".") {
						conditional = false
					}
				}
			}
			if !conditional {
				sort.Strings(keys)
			}
		case '[':
			s[pointer] = value
		}
	case string:
		s[pointer] = ""
	default:
		s[pointer] = value
	}
}

func childRole(role, key string) string {
	switch {
	case role == "root":
		if r, ok := roleOfField[key]; ok {
			return r
		}
		return "keep"
	case role == "js" && key == "types":
		return "dts"
	}
	return role
}

// One open object or array of the manifest being copied.
type manifestFrame struct {
	object      bool
	role        string
	key         bool
	count       int
	sawType     bool
	path        string
	valuePath   string
	browserKeys map[string]bool
}

func manifestAsBuilt(data []byte, tsx string) ([]byte, error) {
	return walkManifest(data, true, func(target, role, pointer string, key bool) (string, error) {
		if key {
			return target, nil
		}
		return emittedFile(target, role, pointer, tsx)
	}, nil)
}

func walkManifest(data []byte, defaultModule bool, transform func(string, string, string, bool) (string, error), visit func(json.Token, string, bool)) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out bytes.Buffer
	var stack []*manifestFrame
	role := "root"
	valuePath := ""
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(stack) == 0 {
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return nil, errors.New("package.json is not an object")
			}
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			top := stack[len(stack)-1]
			if visit != nil {
				visit(tok, top.path, false)
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 && !top.sawType && defaultModule {
				if top.count > 0 {
					out.WriteByte(',')
				}
				out.WriteString(`"type":"module"`)
			}
			out.WriteByte(byte(d))
			if len(stack) > 0 {
				stack[len(stack)-1].key = stack[len(stack)-1].object
			}
			continue
		}
		var top *manifestFrame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.key {
			key := tok.(string)
			if top.path == "/browser" {
				if strings.HasPrefix(key, "./") || strings.HasPrefix(key, "../") {
					key, err = transform(key, "js", top.path, true)
					if err != nil {
						return nil, err
					}
				}
				if top.browserKeys == nil {
					top.browserKeys = map[string]bool{}
				}
				if top.browserKeys[key] {
					return nil, fmt.Errorf("browser key collision at %q; use distinct runtime file keys", key)
				}
				top.browserKeys[key] = true
			}
			if visit != nil {
				visit(key, top.path, true)
			}
			if top.count > 0 {
				out.WriteByte(',')
			}
			top.count++
			out.WriteString(quoteJSON(key) + ":")
			role = childRole(top.role, key)
			top.valuePath = top.path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			if len(stack) == 1 && key == "type" {
				top.sawType = true
			}
			top.key = false
			continue
		}
		if top != nil && !top.object {
			if top.count > 0 {
				out.WriteByte(',')
			}
			top.valuePath = fmt.Sprintf("%s/%d", top.path, top.count)
			top.count++
			role = top.role
		}
		if top != nil {
			valuePath = top.valuePath
		}
		if visit != nil {
			visit(tok, valuePath, false)
		}
		switch v := tok.(type) {
		case json.Delim:
			out.WriteByte(byte(v))
			stack = append(stack, &manifestFrame{
				object: v == '{',
				role:   role,
				key:    v == '{',
				path:   valuePath,
			})
			continue
		case string:
			projected, err := transform(v, role, valuePath, false)
			if err != nil {
				return nil, err
			}
			out.WriteString(quoteJSON(projected))
		case json.Number:
			out.WriteString(v.String())
		case bool:
			fmt.Fprint(&out, v)
		case nil:
			out.WriteString("null")
		}
		if top != nil && top.object {
			top.key = true
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("package.json ends inside a value")
	}
	return out.Bytes(), nil
}

// quoteJSON is the string's JSON form with < > & unescaped, as tsc and node
// read them either way.
func quoteJSON(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
