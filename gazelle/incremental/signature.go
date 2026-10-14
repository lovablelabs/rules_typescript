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

// Scanned is one source's specifiers and rest, as Specifiers returns them, and
// Resolutions, how many times tsgo resolves each specifier text; Counted is false
// when the scanner cannot settle one of those counts.
type Scanned struct {
	Specs       []string
	Rest        string
	Resolutions map[string]int
	Counted     bool
}

// Scan reads a source once for Specifiers and Resolutions. tsgo resolves a dynamic
// import again for each "import" or "require" in its span, a comment before it included.
func Scan(name string, src []byte) (Scanned, error) {
	s, err := scanSource(name, src)
	if err != nil {
		return Scanned{}, err
	}
	var specs, others []string
	for _, fact := range s.facts {
		switch kind, spec, _ := strings.Cut(fact, " "); {
		case kind == "import", kind == "jsdoc" && s.js:
			specs = append(specs, "import:"+spec[1:len(spec)-1])
		case kind == "require":
			specs = append(specs, "require:"+spec[1:len(spec)-1])
		default:
			others = append(others, fact)
		}
	}
	sort.Strings(specs)
	sort.Strings(others)
	return Scanned{Specs: specs, Rest: restDigest(s.sawJSX, s.module, others), Resolutions: s.counts, Counted: !s.uncounted}, nil
}

// Specifiers are the module specifiers tsgo resolves in a source, as sorted
// "import:x" or "require:x"; rest digests everything else its Signature covers.
func Specifiers(name string, src []byte) (specs []string, rest string, err error) {
	scanned, err := Scan(name, src)
	return scanned.Specs, scanned.Rest, err
}

// PlainRest is the rest Specifiers returns for a source whose only facts are
// its module specifiers.
func PlainRest(jsx, module bool) string {
	return restDigest(jsx, module, nil)
}

func restDigest(jsx, module bool, others []string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("jsx=%t module=%t\n%s", jsx, module, strings.Join(others, "\n"))))
	return hex.EncodeToString(sum[:])
}

func scanSource(name string, src []byte) (*scanner, error) {
	ext := path.Ext(name)
	s := &scanner{src: string(src), jsx: ext == ".tsx" || ext == ".jsx" || ext == ".js" || ext == ".mjs" || ext == ".cjs",
		js: !strings.Contains(ext, "t")}
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
	end  int
}

// callSpan is a dynamic import or require call from the end of the token before
// it to its `)`, or to the name after it when it may be an import type's qualifier.
type callSpan struct {
	open       bool
	text       string
	start, end int
	dot        bool
	// uncertain is a mention in the specifier or the qualifier, which tsgo
	// counts in an import call but not in an import type.
	uncertain bool
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
	js     bool
	call   callSpan
	counts map[string]int
	// uncounted: some specifier's resolution count is unknown.
	uncounted bool
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
	t.end = s.pos
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
	s.followCall(t)
	if t.kind == 'i' && (t.text == "import" || t.text == "export") && s.depth == 0 {
		s.module = true
	}
	call := func() bool {
		l := s.last(3)
		return l != nil && l[1].text == "(" && l[0].kind == 'i' && (l[0].text == "require" || l[0].text == "import")
	}
	if t.kind == 't' && call() {
		// tsgo resolves a template without substitutions, which no fact records.
		s.uncounted = true
	}
	if t.kind != 's' {
		return
	}
	if l := s.last(2); l != nil && l[0].kind == 'i' && (l[0].text == "from" || l[0].text == "import") {
		s.facts = append(s.facts, "import "+t.text)
		s.count(t.text[1:len(t.text)-1], 1)
		return
	}
	if call() {
		keyword := s.last(3)[0].text
		s.facts = append(s.facts, keyword+" "+t.text)
		s.startCall(keyword, t)
		return
	}
	if l := s.last(3); l != nil && l[0].text == "declare" && l[1].text == "module" {
		s.facts = append(s.facts, "augment "+t.text)
	}
}

func (s *scanner) count(text string, n int) {
	if s.counts == nil {
		s.counts = map[string]int{}
	}
	s.counts[text] += n
}

func (s *scanner) startCall(keyword string, specifier token) {
	text := specifier.text[1 : len(specifier.text)-1]
	if keyword == "require" && !s.js {
		// TypeScript resolves only an import-equals declaration's require.
		if l := s.last(6); l != nil && l[2].text == "=" && l[1].kind == 'i' && (l[0].text == "import" || l[0].text == "type") {
			s.count(text, 1)
		} else {
			s.uncounted = true
		}
		return
	}
	if l := s.last(4); l != nil && (l[0].text == "." || l[0].text == "?.") {
		// A method named import or require is no call tsgo resolves.
		s.uncounted = true
		return
	}
	start := 0
	if l := s.last(4); l != nil {
		start = l[0].end
		if l[0].text == "typeof" {
			start = 0
			if l := s.last(5); l != nil {
				start = l[0].end
			}
		}
	}
	s.call = callSpan{open: true, text: text, start: start, uncertain: mentionsImport(specifier.text)}
}

// followCall reads the tokens after a call's specifier: its `)`, and a name a dot
// puts after it; anything else in the call leaves its count unsettled.
func (s *scanner) followCall(t token) {
	c := &s.call
	switch {
	case !c.open:
	case c.end == 0 && t.kind == 'p' && t.text == ")":
		c.end = t.end
	case c.end == 0:
		s.uncounted, c.open = true, false
	case !c.dot && t.kind == 'p' && t.text == "<":
		s.uncounted, c.open = true, false
	case !c.dot && t.kind == 'p' && t.text == ".":
		c.dot = true
	case c.dot && t.kind == 'i':
		c.end = t.end
		c.uncertain = c.uncertain || mentionsImport(t.text)
		s.endCall()
	default:
		s.endCall()
	}
}

func (s *scanner) endCall() {
	c := s.call
	s.call = callSpan{}
	switch {
	case !c.open:
	case c.end == 0, c.uncertain:
		s.uncounted = true
	default:
		// tsgo's search stops at each "import" or "require", left to right without overlap.
		span := s.src[c.start:c.end]
		s.count(c.text, strings.Count(span, "import")+strings.Count(span, "require"))
	}
}

func mentionsImport(text string) bool {
	return strings.Contains(text, "import") || strings.Contains(text, "require")
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func (s *scanner) scan() error {
	if err := s.scanJS(-1); err != nil {
		return err
	}
	s.endCall()
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
			s.emit(token{kind: 'p', text: "}"})
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
			s.emit(token{kind: 's', text: text})
		case c == '`':
			s.pos++
			done, err := s.templateBody()
			if err != nil {
				return err
			}
			if done {
				s.emit(token{kind: 't', text: "`"})
			} else {
				templates = append(templates, s.depth)
				s.depth++
				s.emit(token{kind: 'p', text: "${"})
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
				s.emit(token{kind: 't', text: "`"})
			} else {
				templates = append(templates, s.depth)
				s.depth++
				s.emit(token{kind: 'p', text: "${"})
			}
		case c == '/' && !s.afterOperand():
			if err := s.regex(); err != nil {
				return err
			}
			s.emit(token{kind: 's', text: "/regex/"})
		case c == '<' && s.jsx && !s.afterOperand():
			if s.genericArrow() {
				s.pos++
				s.emit(token{kind: 'p', text: "<"})
				continue
			}
			s.sawJSX = true
			if err := s.jsxElement(); err != nil {
				return err
			}
			s.emit(token{kind: 'n', text: "jsx"})
		case isIdentStart(c):
			start := s.pos
			for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
				s.pos++
			}
			s.emit(token{kind: 'i', text: s.src[start:s.pos]})
		case c >= '0' && c <= '9' || c == '.' && s.pos+1 < len(s.src) && s.src[s.pos+1] >= '0' && s.src[s.pos+1] <= '9':
			for s.pos < len(s.src) && (isIdentPart(s.src[s.pos]) || s.src[s.pos] == '.') {
				s.pos++
			}
			s.emit(token{kind: 'n', text: "0"})
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
			s.emit(token{kind: 'p', text: text})
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
	if s.js && mentionsImport(text) {
		// JavaScript's JSDoc is parsed, and an import type in it is resolved.
		s.uncounted = true
	}
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
