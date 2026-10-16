package typescript

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/mikn/rules_typescript/ts/tools/explainfiles"
	"github.com/mikn/rules_typescript/ts/tools/tsconfig"
)

// listingFormat opens every listing cache file: a file of another format is a miss.
const listingFormat = "rules_typescript listing cache 4\n"

// A field added to an encoded struct breaks these conversions until the codec writes it.
var (
	_ = cachedListing(struct {
		Listing               explainfiles.Listing
		Candidates            []cachedCandidate
		Reads, Dirs           []string
		Configs               []string
		PackageExtends        bool
		Fingerprint           string
		Root                  string
		ReadLines, DirDigests []string
		Sources               map[string]cachedSource
		PatchVersion          int
		Resolution            resolutionFacts
		Config                *patchConfig
		Unmatched             []string
		FilesList             []string
		StaleDiagnostics      bool
		Refused               string
	}{})
	_ = cachedCandidate(struct {
		From, Specifier, Path string
		File, Metadata        bool
		Kind                  explainfiles.EdgeKind
		Block                 int
		Resolved, Skipped     string
	}{})
	_ = cachedSource(struct {
		Specs          []string
		Rest           string
		Sum            uint64
		ResolutionMode bool
	}{})
	_ = resolutionFacts(struct {
		Kinds, RelativeTargets             []string
		RootDirs                           bool
		Substitutions, SubstitutionTargets []string
	}{})
	_ = patchConfig(struct {
		Include, Exclude       []string
		IncludeDir, ExcludeDir string
		ModuleSuffixes         bool
		Refused                string
		Paths                  []tsconfig.Path
		PathsDir               string
	}{})
	_ = tsconfig.Path(struct {
		Key    string
		Values []string
	}{})
	_ = explainfiles.Listing(struct {
		Files          []string
		Roots          []string
		Edges          []explainfiles.Edge
		Types          []explainfiles.TypeEntry
		Implicit       []explainfiles.TypeEntry
		Diagnostics    []string
		Unresolved     []explainfiles.Unresolved
		Resolutions    []explainfiles.Resolution
		Candidates     []explainfiles.ResolutionCandidate
		PackageTargets []explainfiles.Edge
	}{})
	_ = explainfiles.Edge(struct {
		Kind                explainfiles.EdgeKind
		From, To, Specifier string
	}{})
	_ = explainfiles.TypeEntry(struct{ Entry, File string }{})
	_ = explainfiles.Unresolved(struct {
		Kind            explainfiles.EdgeKind
		From, Specifier string
		Candidates      []explainfiles.FailedLookup
	}{})
	_ = explainfiles.FailedLookup(struct{ File, Candidate string }{})
	_ = explainfiles.Resolution(struct {
		explainfiles.Edge
		Candidates    []explainfiles.FailedLookup
		Substitutions []string
		FromPackage   bool
	}{})
	_ = explainfiles.ResolutionCandidate(struct {
		Kind                  explainfiles.EdgeKind
		From, Specifier, Path string
	}{})
)

var errListingFormat = errors.New("not a listing cache entry of this version")

type listingEncoder struct {
	w   *bufio.Writer
	ids map[string]uint64
	tmp [binary.MaxVarintLen64]byte
}

var listingEncoders = sync.Pool{New: func() any {
	return &listingEncoder{w: bufio.NewWriterSize(nil, 64<<10), ids: map[string]uint64{}}
}}

// Each string is written once, then named by its number; 1 is "".
func encodeEntry(w io.Writer, entry *cachedListing) error {
	return encodeListing(w, entry, true)
}

// encodeListing writes entry, only its head unless rest is set.
func encodeListing(w io.Writer, entry *cachedListing, rest bool) error {
	e := listingEncoders.Get().(*listingEncoder)
	defer listingEncoders.Put(e)
	e.w.Reset(w)
	defer e.w.Reset(nil)
	clear(e.ids)
	e.ids[""] = 1
	e.w.WriteString(listingFormat)
	e.head(entry)
	if rest {
		e.rest(entry)
	}
	return e.w.Flush()
}

func (e *listingEncoder) uint(v uint64) {
	e.w.Write(binary.AppendUvarint(e.tmp[:0], v))
}

func (e *listingEncoder) int(v int) {
	e.w.Write(binary.AppendVarint(e.tmp[:0], int64(v)))
}

func (e *listingEncoder) bool(b bool) {
	if b {
		e.w.WriteByte(1)
	} else {
		e.w.WriteByte(0)
	}
}

func (e *listingEncoder) string(s string) {
	if id, ok := e.ids[s]; ok {
		e.uint(id)
		return
	}
	e.ids[s] = uint64(len(e.ids)) + 1
	e.uint(0)
	e.uint(uint64(len(s)))
	e.w.WriteString(s)
}

func (e *listingEncoder) strings(list []string) {
	encodeList(e, list, e.string)
}

func encodeList[T any](e *listingEncoder, list []T, each func(T)) {
	e.uint(uint64(len(list)))
	for _, v := range list {
		each(v)
	}
}

func (e *listingEncoder) edge(edge explainfiles.Edge) {
	e.int(int(edge.Kind))
	e.string(edge.From)
	e.string(edge.To)
	e.string(edge.Specifier)
}

func (e *listingEncoder) typeEntry(t explainfiles.TypeEntry) {
	e.string(t.Entry)
	e.string(t.File)
}

func (e *listingEncoder) failedLookup(f explainfiles.FailedLookup) {
	e.string(f.File)
	e.string(f.Candidate)
}

// head is what the fast path declines on before it decodes the rest of an entry.
func (e *listingEncoder) head(l *cachedListing) {
	e.string(l.Root)
	// As gob did, an empty map stays apart from none.
	if l.Sources == nil {
		e.uint(0)
	} else {
		e.uint(uint64(len(l.Sources)) + 1)
	}
	for file, src := range l.Sources {
		e.string(file)
		e.strings(src.Specs)
		e.string(src.Rest)
		e.uint(src.Sum)
		e.bool(src.ResolutionMode)
	}
	e.strings(l.FilesList)
	encodeList(e, l.Listing.Edges, e.edge)
}

func (e *listingEncoder) rest(l *cachedListing) {
	e.strings(l.Listing.Files)
	e.strings(l.Listing.Roots)
	encodeList(e, l.Listing.Types, e.typeEntry)
	encodeList(e, l.Listing.Implicit, e.typeEntry)
	e.strings(l.Listing.Diagnostics)
	encodeList(e, l.Listing.Unresolved, func(u explainfiles.Unresolved) {
		e.int(int(u.Kind))
		e.string(u.From)
		e.string(u.Specifier)
		encodeList(e, u.Candidates, e.failedLookup)
	})
	encodeList(e, l.Listing.Resolutions, func(r explainfiles.Resolution) {
		e.edge(r.Edge)
		encodeList(e, r.Candidates, e.failedLookup)
		e.strings(r.Substitutions)
		e.bool(r.FromPackage)
	})
	encodeList(e, l.Listing.Candidates, func(c explainfiles.ResolutionCandidate) {
		e.int(int(c.Kind))
		e.string(c.From)
		e.string(c.Specifier)
		e.string(c.Path)
	})
	encodeList(e, l.Listing.PackageTargets, e.edge)
	encodeList(e, l.Candidates, func(c cachedCandidate) {
		e.string(c.From)
		e.string(c.Specifier)
		e.string(c.Path)
		e.bool(c.File)
		e.bool(c.Metadata)
		e.int(int(c.Kind))
		e.int(c.Block)
		e.string(c.Resolved)
		e.string(c.Skipped)
	})
	e.strings(l.Reads)
	e.strings(l.Dirs)
	e.strings(l.Configs)
	e.bool(l.PackageExtends)
	e.string(l.Fingerprint)
	e.strings(l.ReadLines)
	e.strings(l.DirDigests)
	e.int(l.PatchVersion)
	e.strings(l.Resolution.Kinds)
	e.strings(l.Resolution.RelativeTargets)
	e.bool(l.Resolution.RootDirs)
	e.strings(l.Resolution.Substitutions)
	e.strings(l.Resolution.SubstitutionTargets)
	e.bool(l.Config != nil)
	if c := l.Config; c != nil {
		e.strings(c.Include)
		e.strings(c.Exclude)
		e.string(c.IncludeDir)
		e.string(c.ExcludeDir)
		e.bool(c.ModuleSuffixes)
		e.string(c.Refused)
		encodeList(e, c.Paths, func(p tsconfig.Path) {
			e.string(p.Key)
			e.strings(p.Values)
		})
		e.string(c.PathsDir)
	}
	e.strings(l.Unmatched)
	e.bool(l.StaleDiagnostics)
	e.string(l.Refused)
}

type listingDecoder struct {
	r *bufio.Reader
	// size bounds every length: each element of a list takes at least a byte.
	size  uint64
	names []string
	buf   []byte
	err   error
}

// A corrupt length allocates at most this much ahead of the elements it reads.
const maxPrealloc = 1 << 16

var listingDecoders = sync.Pool{New: func() any { return &listingDecoder{r: bufio.NewReaderSize(nil, 64<<10)} }}

// openListing opens file, waiting for an entry still being written.
func openListing(file string) (*os.File, uint64, error) {
	if written, ok := pendingSaves.Load(file); ok {
		<-written.(chan struct{})
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, uint64(info.Size()), nil
}

// readListingSum decodes file's entry, and its sha256 when sum is set.
func readListingSum(file string, sum bool) (*cachedListing, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, size, err := openListing(file)
	if err != nil {
		return nil, digest, err
	}
	defer f.Close()
	var r io.Reader = f
	var h hash.Hash
	if sum {
		h = sha256.New()
		r = io.TeeReader(f, h)
	}
	entry, err := decodeEntry(r, size)
	if err != nil {
		return nil, digest, fmt.Errorf("%s: %w", file, err)
	}
	if sum {
		h.Sum(digest[:0])
	}
	return entry, digest, nil
}

func readListing(file string) (*cachedListing, error) {
	entry, _, err := readListingSum(file, false)
	return entry, err
}

// decodeEntry interns every string it reads, so entries share the paths they name.
func decodeEntry(r io.Reader, size uint64) (*cachedListing, error) {
	d := listingDecoders.Get().(*listingDecoder)
	defer listingDecoders.Put(d)
	if err := d.begin(r, size); err != nil {
		return nil, err
	}
	defer d.release()
	entry := d.head()
	d.rest(entry)
	if err := d.end(); err != nil {
		return nil, err
	}
	return entry, nil
}

// begin reads r's header; release drops what the decode holds.
func (d *listingDecoder) begin(r io.Reader, size uint64) error {
	d.r.Reset(r)
	d.size, d.err = size, nil
	d.names = append(d.names[:0], "")
	if format, err := d.r.Peek(len(listingFormat)); err != nil || string(format) != listingFormat {
		d.release()
		return errListingFormat
	}
	d.r.Discard(len(listingFormat))
	return nil
}

func (d *listingDecoder) release() {
	d.r.Reset(nil)
	clear(d.names)
}

// end is the decode's error, past a check that the entry ends where its file does.
func (d *listingDecoder) end() error {
	if _, err := d.r.ReadByte(); d.err == nil && err == nil {
		d.err = errors.New("trailing bytes")
	} else if err != io.EOF {
		d.fail(err)
	}
	if d.err != nil {
		return fmt.Errorf("corrupt listing cache entry: %w", d.err)
	}
	return nil
}

// An entryHead holds an entry's decoder from its head to the rest of it; rest or close releases it.
type entryHead struct {
	file  string
	d     *listingDecoder
	entry *cachedListing
}

// decodeHead decodes file's entry as far as its head, which holds Root, Sources, FilesList and Listing.Edges.
// It reads the whole file, so the heads of many entries hold no file open.
func decodeHead(file string) (*entryHead, error) {
	f, size, err := openListing(file)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	_, err = io.ReadFull(f, data)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	// Its own buffer, no larger than the file, as the fast path holds every head at once.
	d := &listingDecoder{r: bufio.NewReaderSize(nil, int(min(size, 64<<10)))}
	if err := d.begin(bytes.NewReader(data), size); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	h := &entryHead{file: file, d: d, entry: d.head()}
	if d.err != nil {
		err := fmt.Errorf("%s: corrupt listing cache entry: %w", file, d.err)
		h.close()
		return nil, err
	}
	return h, nil
}

// rest decodes the rest of the entry into the head's.
func (h *entryHead) rest() (*cachedListing, error) {
	defer h.close()
	h.d.rest(h.entry)
	if err := h.d.end(); err != nil {
		return nil, fmt.Errorf("%s: %w", h.file, err)
	}
	return h.entry, nil
}

func (h *entryHead) close() {
	if h.d != nil {
		h.d.release()
		h.d = nil
	}
}

func (d *listingDecoder) fail(err error) {
	if d.err == nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		d.err = err
	}
}

func (d *listingDecoder) uint() uint64 {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(d.r)
	if err != nil {
		d.fail(err)
	}
	return v
}

func (d *listingDecoder) int() int {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(d.r)
	if err != nil {
		d.fail(err)
	}
	return int(v)
}

func (d *listingDecoder) bool() bool {
	if d.err != nil {
		return false
	}
	b, err := d.r.ReadByte()
	if err != nil {
		d.fail(err)
	} else if b > 1 {
		d.fail(errors.New("not a bool"))
	}
	return b == 1
}

func (d *listingDecoder) length() int {
	n := d.uint()
	if n > d.size {
		d.fail(errors.New("a length past the end of the file"))
		return 0
	}
	return int(n)
}

func (d *listingDecoder) string() string {
	id := d.uint()
	if d.err != nil {
		return ""
	}
	if id > 0 {
		if id > uint64(len(d.names)) {
			d.fail(errors.New("an unknown string"))
			return ""
		}
		return d.names[id-1]
	}
	n := d.length()
	if d.err != nil {
		return ""
	}
	var b []byte
	if n <= d.r.Size() {
		var err error
		if b, err = d.r.Peek(n); err != nil {
			d.fail(err)
			return ""
		}
		defer d.r.Discard(n)
	} else {
		if cap(d.buf) < n {
			d.buf = make([]byte, n)
		}
		d.buf = d.buf[:n]
		if _, err := io.ReadFull(d.r, d.buf); err != nil {
			d.fail(err)
			return ""
		}
		b = d.buf
	}
	s := internBytes(b)
	d.names = append(d.names, s)
	return s
}

func internBytes(b []byte) string {
	if v, ok := listingStrings.Load(unsafe.String(unsafe.SliceData(b), len(b))); ok {
		return v.(string)
	}
	kept := string(b)
	v, _ := listingStrings.LoadOrStore(kept, kept)
	return v.(string)
}

func (d *listingDecoder) strings() []string {
	return decodeList(d, d.string)
}

func decodeList[T any](d *listingDecoder, each func() T) []T {
	n := d.length()
	if n == 0 || d.err != nil {
		return nil
	}
	list := make([]T, 0, min(n, maxPrealloc))
	for range n {
		if list = append(list, each()); d.err != nil {
			return nil
		}
	}
	return list
}

func (d *listingDecoder) edge() explainfiles.Edge {
	return explainfiles.Edge{Kind: explainfiles.EdgeKind(d.int()), From: d.string(), To: d.string(), Specifier: d.string()}
}

func (d *listingDecoder) typeEntry() explainfiles.TypeEntry {
	return explainfiles.TypeEntry{Entry: d.string(), File: d.string()}
}

func (d *listingDecoder) failedLookup() explainfiles.FailedLookup {
	return explainfiles.FailedLookup{File: d.string(), Candidate: d.string()}
}

func (d *listingDecoder) head() *cachedListing {
	l := &cachedListing{}
	l.Root = d.string()
	if n := d.length(); n > 0 {
		l.Sources = make(map[string]cachedSource, min(n-1, maxPrealloc))
		for range n - 1 {
			file := d.string()
			l.Sources[file] = cachedSource{Specs: d.strings(), Rest: d.string(), Sum: d.uint(), ResolutionMode: d.bool()}
		}
	}
	l.FilesList = d.strings()
	l.Listing.Edges = decodeList(d, d.edge)
	return l
}

// rest decodes the rest of the entry whose head is l into l.
func (d *listingDecoder) rest(l *cachedListing) {
	l.Listing.Files = d.strings()
	l.Listing.Roots = d.strings()
	l.Listing.Types = decodeList(d, d.typeEntry)
	l.Listing.Implicit = decodeList(d, d.typeEntry)
	l.Listing.Diagnostics = d.strings()
	l.Listing.Unresolved = decodeList(d, func() explainfiles.Unresolved {
		return explainfiles.Unresolved{Kind: explainfiles.EdgeKind(d.int()), From: d.string(), Specifier: d.string(), Candidates: decodeList(d, d.failedLookup)}
	})
	l.Listing.Resolutions = decodeList(d, func() explainfiles.Resolution {
		return explainfiles.Resolution{Edge: d.edge(), Candidates: decodeList(d, d.failedLookup), Substitutions: d.strings(), FromPackage: d.bool()}
	})
	l.Listing.Candidates = decodeList(d, func() explainfiles.ResolutionCandidate {
		return explainfiles.ResolutionCandidate{Kind: explainfiles.EdgeKind(d.int()), From: d.string(), Specifier: d.string(), Path: d.string()}
	})
	l.Listing.PackageTargets = decodeList(d, d.edge)
	l.Candidates = decodeList(d, func() cachedCandidate {
		return cachedCandidate{From: d.string(), Specifier: d.string(), Path: d.string(), File: d.bool(), Metadata: d.bool(),
			Kind: explainfiles.EdgeKind(d.int()), Block: d.int(), Resolved: d.string(), Skipped: d.string()}
	})
	l.Reads = d.strings()
	l.Dirs = d.strings()
	l.Configs = d.strings()
	l.PackageExtends = d.bool()
	l.Fingerprint = d.string()
	l.ReadLines = d.strings()
	l.DirDigests = d.strings()
	l.PatchVersion = d.int()
	l.Resolution = resolutionFacts{Kinds: d.strings(), RelativeTargets: d.strings(), RootDirs: d.bool(),
		Substitutions: d.strings(), SubstitutionTargets: d.strings()}
	if d.bool() {
		l.Config = &patchConfig{Include: d.strings(), Exclude: d.strings(), IncludeDir: d.string(), ExcludeDir: d.string(),
			ModuleSuffixes: d.bool(), Refused: d.string(), Paths: decodeList(d, func() tsconfig.Path {
				return tsconfig.Path{Key: d.string(), Values: d.strings()}
			}), PathsDir: d.string()}
	}
	l.Unmatched = d.strings()
	l.StaleDiagnostics = d.bool()
	l.Refused = d.string()
}
