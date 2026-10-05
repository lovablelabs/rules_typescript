package incremental

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// errUnscannable is a source the scanner cannot prove it read correctly; the
// caller then treats the file as changed in every way.
var errUnscannable = errors.New("source the signature scanner cannot read")

// A JSDoc import type or @import tag, which tsgo resolves in a JavaScript file.
var jsdocImport = regexp.MustCompile(`(?:import\(\s*|@import\b[^'"]*?from\s*)(['"][^'"]*['"])`)

// Specifiers are the module specifiers tsgo resolves in a source, as sorted
// "import:x" or "require:x"; rest digests everything else its Signature covers.
func Specifiers(name string, src []byte) (specs []string, rest string, err error) {
	s, err := scanSource(name, src)
	if err != nil {
		return nil, "", err
	}
	js := !strings.Contains(path.Ext(name), "t")
	var others []string
	for _, fact := range s.facts {
		switch kind, spec, _ := strings.Cut(fact, " "); {
		case kind == "import", kind == "jsdoc" && js:
			specs = append(specs, "import:"+spec[1:len(spec)-1])
		case kind == "require":
			specs = append(specs, "require:"+spec[1:len(spec)-1])
		default:
			others = append(others, fact)
		}
	}
	sort.Strings(specs)
	sort.Strings(others)
	sum := sha256.Sum256([]byte(fmt.Sprintf("jsx=%t module=%t\n%s", s.sawJSX, s.module, strings.Join(others, "\n"))))
	return specs, hex.EncodeToString(sum[:]), nil
}

func scanSource(name string, src []byte) (*scanner, error) {
	ext := path.Ext(name)
	s := &scanner{src: string(src), jsx: ext == ".tsx" || ext == ".jsx" || ext == ".js" || ext == ".mjs" || ext == ".cjs"}
	return s, s.scan()
}

// Signature is what a source contributes to its compiler listing; an edit that
// keeps it keeps the listing, so it cannot change a generated BUILD.
func Signature(name string, src []byte) (string, error) {
	s, err := scanSource(name, src)
	if err != nil {
		return "", err
	}
	sort.Strings(s.facts)
	sum := sha256.Sum256([]byte(fmt.Sprintf("jsx=%t module=%t\n%s", s.sawJSX, s.module, strings.Join(s.facts, "\n"))))
	return hex.EncodeToString(sum[:]), nil
}

type token struct {
	kind byte // 'i' identifier or keyword, 's' string, 'n' number, 'p' punctuation, 't' template end
	text string
}

type scanner struct {
	src    string
	pos    int
	jsx    bool
	prev   token
	toks   []token
	facts  []string
	sawJSX bool
	module bool
	depth  int
}

var keywordsBeforeExpression = map[string]bool{
	"return": true, "yield": true, "await": true, "case": true, "typeof": true, "void": true,
	"delete": true, "throw": true, "new": true, "in": true, "of": true, "instanceof": true,
	"else": true, "do": true, "default": true, "extends": true,
}

var valueKeywords = map[string]bool{"this": true, "super": true, "null": true, "true": true, "false": true}

// afterOperand is whether the previous token ends an operand, so `<` and `/`
// are operators rather than the start of JSX or a regular expression.
func (s *scanner) afterOperand() bool {
	switch s.prev.kind {
	case 0:
		return false
	case 'i':
		return !keywordsBeforeExpression[s.prev.text] || valueKeywords[s.prev.text]
	case 's', 'n', 't':
		return true
	case 'p':
		return s.prev.text == ")" || s.prev.text == "]"
	}
	return false
}

func (s *scanner) emit(t token) {
	s.prev = t
	s.toks = append(s.toks, t)
	if len(s.toks) > 8 {
		s.toks = s.toks[len(s.toks)-8:]
	}
	s.recognize()
}

func (s *scanner) last(n int) []token {
	if len(s.toks) < n {
		return nil
	}
	return s.toks[len(s.toks)-n:]
}

// recognize records a fact when the newest token completes one.
func (s *scanner) recognize() {
	t := s.toks[len(s.toks)-1]
	if t.kind == 'i' && (t.text == "import" || t.text == "export") && s.depth == 0 {
		s.module = true
	}
	if t.kind != 's' {
		return
	}
	if l := s.last(2); l != nil && l[0].kind == 'i' && (l[0].text == "from" || l[0].text == "import") {
		s.facts = append(s.facts, "import "+t.text)
		return
	}
	if l := s.last(3); l != nil && l[1].text == "(" && l[0].kind == 'i' && (l[0].text == "require" || l[0].text == "import") {
		s.facts = append(s.facts, l[0].text+" "+t.text)
		return
	}
	if l := s.last(3); l != nil && l[0].text == "declare" && l[1].text == "module" {
		s.facts = append(s.facts, "augment "+t.text)
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func (s *scanner) scan() error {
	if err := s.scanJS(-1); err != nil {
		return err
	}
	if s.depth != 0 {
		return errUnscannable
	}
	return nil
}

// scanJS scans JavaScript until the `}` that closes depth stop, or to the end
// of the source when stop is negative.
func (s *scanner) scanJS(stop int) error {
	var templates []int // brace depth at which each open template's ${ resumes
	for s.pos < len(s.src) {
		if stop >= 0 && s.src[s.pos] == '}' && s.depth == stop+1 && len(templates) == 0 {
			s.depth--
			s.pos++
			s.emit(token{'p', "}"})
			return nil
		}
		c := s.src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			s.pos++
		case c == '/' && s.at("//"):
			end := strings.IndexByte(s.src[s.pos:], '\n')
			if end < 0 {
				end = len(s.src) - s.pos
			}
			s.comment(s.src[s.pos : s.pos+end])
			s.pos += end
		case c == '/' && s.at("/*"):
			end := strings.Index(s.src[s.pos+2:], "*/")
			if end < 0 {
				return errUnscannable
			}
			s.comment(s.src[s.pos : s.pos+2+end+2])
			s.pos += end + 4
		case c == '\'' || c == '"':
			text, err := s.quoted(c)
			if err != nil {
				return err
			}
			s.emit(token{'s', text})
		case c == '`':
			s.pos++
			done, err := s.templateBody()
			if err != nil {
				return err
			}
			if done {
				s.emit(token{'t', "`"})
			} else {
				templates = append(templates, s.depth)
				s.depth++
				s.emit(token{'p', "${"})
			}
		case c == '}' && len(templates) > 0 && templates[len(templates)-1] == s.depth-1:
			s.depth--
			templates = templates[:len(templates)-1]
			s.pos++
			done, err := s.templateBody()
			if err != nil {
				return err
			}
			if done {
				s.emit(token{'t', "`"})
			} else {
				templates = append(templates, s.depth)
				s.depth++
				s.emit(token{'p', "${"})
			}
		case c == '/' && !s.afterOperand():
			if err := s.regex(); err != nil {
				return err
			}
			s.emit(token{'s', "/regex/"})
		case c == '<' && s.jsx && !s.afterOperand():
			if s.genericArrow() {
				s.pos++
				s.emit(token{'p', "<"})
				continue
			}
			s.sawJSX = true
			if err := s.jsxElement(); err != nil {
				return err
			}
			s.emit(token{'n', "jsx"})
		case isIdentStart(c):
			start := s.pos
			for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
				s.pos++
			}
			s.emit(token{'i', s.src[start:s.pos]})
		case c >= '0' && c <= '9' || c == '.' && s.pos+1 < len(s.src) && s.src[s.pos+1] >= '0' && s.src[s.pos+1] <= '9':
			for s.pos < len(s.src) && (isIdentPart(s.src[s.pos]) || s.src[s.pos] == '.') {
				s.pos++
			}
			s.emit(token{'n', "0"})
		default:
			text := string(c)
			for _, op := range []string{"=>", "...", "?.", "++", "--"} {
				if s.at(op) {
					text = op
					break
				}
			}
			switch text {
			case "{":
				s.depth++
			case "}":
				s.depth--
			}
			s.pos += len(text)
			s.emit(token{'p', text})
		}
	}
	if len(templates) > 0 || stop >= 0 {
		return errUnscannable
	}
	return nil
}

// jsxElement consumes one element or fragment from its `<`: tag, attributes,
// children text, nested elements, and the JavaScript inside every `{}`.
func (s *scanner) jsxElement() error {
	s.pos++ // <
	closing := false
	angles := 0 // type arguments on the tag: <Component<T> />
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '<':
			angles++
			s.pos++
		case c == '>' && angles > 0:
			angles--
			s.pos++
		case c == '{':
			if err := s.jsxExpression(); err != nil {
				return err
			}
		case c == '"' || c == '\'':
			if _, err := s.quoted(c); err != nil {
				return err
			}
		case c == '/' && s.at("//"):
			end := strings.IndexByte(s.src[s.pos:], '\n')
			if end < 0 {
				return errUnscannable
			}
			s.pos += end
		case c == '/' && s.at("/*"):
			end := strings.Index(s.src[s.pos+2:], "*/")
			if end < 0 {
				return errUnscannable
			}
			s.pos += end + 4
		case c == '/' && s.at("/>"):
			s.pos += 2
			return nil
		case c == '>':
			s.pos++
			if closing {
				return nil
			}
			return s.jsxChildren()
		case c == '/':
			closing = true
			s.pos++
		default:
			s.pos++
		}
	}
	return errUnscannable
}

// jsxText is text a misread JSX element could have swallowed from real code.
var jsxText = regexp.MustCompile(`\bimport\s*\(|\brequire\s*\(|\bfrom\s*['"]`)

func (s *scanner) jsxChildren() error {
	start := s.pos
	for s.pos < len(s.src) {
		if s.src[s.pos] == '<' || s.src[s.pos] == '{' {
			if jsxText.MatchString(s.src[start:s.pos]) {
				return errUnscannable
			}
		}
		switch {
		case s.at("</"):
			s.pos++
			// The closing tag ends this element.
			for s.pos < len(s.src) && s.src[s.pos] != '>' {
				s.pos++
			}
			if s.pos >= len(s.src) {
				return errUnscannable
			}
			s.pos++
			return nil
		case s.src[s.pos] == '<':
			if err := s.jsxElement(); err != nil {
				return err
			}
			start = s.pos
		case s.src[s.pos] == '{':
			if err := s.jsxExpression(); err != nil {
				return err
			}
			start = s.pos
		default:
			s.pos++
		}
	}
	return errUnscannable
}

func (s *scanner) jsxExpression() error {
	saved := s.prev
	s.pos++
	s.depth++
	s.prev = token{}
	err := s.scanJS(s.depth - 1)
	s.prev = saved
	return err
}

func (s *scanner) at(lit string) bool { return strings.HasPrefix(s.src[s.pos:], lit) }

func (s *scanner) comment(text string) {
	if strings.HasPrefix(text, "///") {
		s.facts = append(s.facts, "directive "+strings.TrimSpace(text))
	}
	for _, m := range jsdocImport.FindAllStringSubmatch(text, -1) {
		s.facts = append(s.facts, "jsdoc "+m[1])
	}
	for _, pragma := range []string{"@jsx", "@ts-nocheck", "@flow"} {
		if i := strings.Index(text, pragma); i >= 0 {
			s.facts = append(s.facts, "pragma "+strings.Fields(text[i:])[0]+" "+strings.Join(strings.Fields(text[i:])[1:min(2, len(strings.Fields(text[i:])))], ""))
		}
	}
}

func (s *scanner) quoted(q byte) (string, error) {
	start := s.pos
	s.pos++
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case q:
			s.pos++
			return s.src[start:s.pos], nil
		case '\n':
			return "", errUnscannable
		default:
			s.pos++
		}
	}
	return "", errUnscannable
}

// templateBody consumes template text after a backtick or a closing `}`, and
// reports whether the template ended rather than opening a ${ substitution.
func (s *scanner) templateBody() (bool, error) {
	for s.pos < len(s.src) {
		switch {
		case s.src[s.pos] == '\\':
			s.pos += 2
		case s.src[s.pos] == '`':
			s.pos++
			return true, nil
		case s.at("${"):
			s.pos += 2
			return false, nil
		default:
			s.pos++
		}
	}
	return false, errUnscannable
}

func (s *scanner) regex() error {
	s.pos++
	inClass := false
	for s.pos < len(s.src) {
		switch c := s.src[s.pos]; {
		case c == '\\':
			s.pos += 2
		case c == '\n':
			return errUnscannable
		case c == '[':
			inClass = true
			s.pos++
		case c == ']':
			inClass = false
			s.pos++
		case c == '/' && !inClass:
			s.pos++
			for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
				s.pos++
			}
			return nil
		default:
			s.pos++
		}
	}
	return errUnscannable
}

// genericArrow is `<T,>` or `<T extends`, a type parameter list a .tsx file
// writes where JSX could start.
func (s *scanner) genericArrow() bool {
	rest := s.src[s.pos+1:]
	i := 0
	for i < len(rest) && strings.IndexByte(" \t\r\n", rest[i]) >= 0 {
		i++
	}
	start := i
	for i < len(rest) && isIdentPart(rest[i]) {
		i++
	}
	if i == start {
		return false
	}
	for i < len(rest) && strings.IndexByte(" \t\r\n", rest[i]) >= 0 {
		i++
	}
	return strings.HasPrefix(rest[i:], ",") || strings.HasPrefix(rest[i:], "extends ")
}
