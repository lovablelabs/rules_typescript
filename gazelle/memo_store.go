package typescript

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	memoStoreName  = "memo"
	memoStoreMagic = "rules_typescript gazelle memo store v3\n"
)

// binaryIdentity names the running executable by its path and a digest of its contents, or is "" when it cannot be read.
func binaryIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%s %x", exe, h.Sum(nil))
}

// memoBuf is the encoding of a store: a string is its length and bytes, a count or an id a varint.
type memoBuf []byte

func (b memoBuf) num(n int) memoBuf { return binary.AppendUvarint(b, uint64(n)) }

func (b memoBuf) str(s string) memoBuf { return append(b.num(len(s)), s...) }

func (b memoBuf) sum(d [sha256.Size]byte) memoBuf { return append(b, d[:]...) }

// stringIDs numbers the strings of a store's string table in the order the entries first use them.
type stringIDs struct {
	ids  map[string]int
	list []string
}

func (s *stringIDs) id(str string) int {
	id, ok := s.ids[str]
	if !ok {
		id = len(s.list)
		s.ids[str] = id
		s.list = append(s.list, str)
	}
	return id
}

// entry appends e: its inputs, each read as category and name ids and digest, each question as oracle id, count,
// argument ids as increasing gaps and digest, and each output as kind, key and name ids and text.
func (b memoBuf) entry(e taskEntry, oracles map[string]int, strs *stringIDs) memoBuf {
	b = b.sum(e.inputs).num(len(e.reads))
	for _, r := range e.reads {
		b = b.num(strs.id(r.category)).num(strs.id(r.name)).sum(r.digest)
	}
	b = b.num(len(e.questions))
	for _, q := range e.questions {
		b = b.num(oracles[q.oracle]).num(len(q.args))
		var last uint32
		for _, arg := range q.args {
			b = b.num(int(arg - last))
			last = arg
		}
		b = b.sum(q.sum)
	}
	b = b.num(len(e.out))
	for _, o := range e.out {
		b = b.num(strs.id(string(o.kind))).num(strs.id(o.key)).num(strs.id(o.name)).str(o.text)
	}
	return b
}

// encodeMemoStore keeps the entries of tasks with nothing in never: the magic, the key, the binary identity, the
// oracle, question and string tables, the index and the entries, closed by a CRC-32 of everything before it.
func encodeMemoStore(key, binaryID string, table questionArgs, entries []taskEntry) []byte {
	names := table.oracles
	if !slices.Contains(names, "owner state") {
		names = append(slices.Clip(names), "owner state")
	}
	oracles := make(map[string]int, len(names))
	for i, name := range names {
		oracles[name] = i
	}
	strs := &stringIDs{ids: map[string]int{}}
	var index, data memoBuf
	stored := 0
	for _, e := range entries {
		if e.never != "" {
			continue
		}
		stored++
		start := len(data)
		data = data.entry(e, oracles, strs)
		index = index.num(strs.id(e.kind)).str(e.key).num(start).num(len(data) - start)
	}
	b := memoBuf(memoStoreMagic).str(key).str(binaryID).num(len(names))
	for _, name := range names {
		b = b.str(name)
	}
	b = b.num(len(table.args))
	for id, args := range table.args {
		b = b.num(int(table.oracleOf[id])).str(args)
	}
	b = b.num(len(strs.list))
	for _, str := range strs.list {
		b = b.str(str)
	}
	b = append(append(b.num(stored), index...), data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// memoReader decodes a store held as one string, whose substrings it hands out; a read past the end or a count the
// bytes cannot hold marks it bad.
type memoReader struct {
	s   string
	bad bool
}

func (r *memoReader) num() int {
	v, n := binary.Uvarint([]byte(r.s[:min(len(r.s), binary.MaxVarintLen64)]))
	if n <= 0 || v > math.MaxInt32 {
		r.bad = true
		return 0
	}
	r.s = r.s[n:]
	return int(v)
}

// count is a length of items that take at least size bytes each, which the rest of the bytes must hold.
func (r *memoReader) count(size int) int {
	n := r.num()
	if n > len(r.s)/size {
		r.bad = true
		return 0
	}
	return n
}

// id is a number below n, the size of the table it indexes.
func (r *memoReader) id(n int) int {
	id := r.num()
	if id >= n {
		r.bad = true
		return 0
	}
	return id
}

func (r *memoReader) str() string {
	n := r.num()
	if r.bad || n > len(r.s) {
		r.bad = true
		return ""
	}
	s := r.s[:n]
	r.s = r.s[n:]
	return s
}

func (r *memoReader) sum() (d [sha256.Size]byte) {
	if len(r.s) < len(d) {
		r.bad = true
		return d
	}
	r.s = r.s[copy(d[:], r.s):]
	return d
}

// questionArgs is what question argument ids name: per id, its oracle and its arguments, each preceded by a 0.
type questionArgs struct {
	oracles  []string
	oracleOf []uint32
	args     []string
}

// memoStore is the entries the last full pass kept: the question table and string table they point into, the
// pointer-free bytes of the entries, and the offset of each task's entry.
type memoStore struct {
	questionArgs
	strs    []string
	entries string
	spans   map[taskID]span
}

type taskID struct{ kind, key string }

type span struct{ off, n int }

// openMemoStore is the store at path, or nil when it is absent, damaged, or another key's or binary's.
func openMemoStore(path, key, binaryID string) *memoStore {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < len(memoStoreMagic)+4 {
		return nil
	}
	body, tail := data[:len(data)-4], data[len(data)-4:]
	if !bytes.HasPrefix(body, []byte(memoStoreMagic)) || crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(tail) {
		return nil
	}
	r := &memoReader{s: string(body[len(memoStoreMagic):])}
	if r.str() != key || r.str() != binaryID {
		return nil
	}
	m := &memoStore{}
	m.oracles = make([]string, r.count(1))
	for i := range m.oracles {
		m.oracles[i] = r.str()
	}
	m.oracleOf = make([]uint32, r.count(2))
	m.args = make([]string, len(m.oracleOf))
	for i := range m.args {
		m.oracleOf[i], m.args[i] = uint32(r.id(len(m.oracles))), r.str()
	}
	m.strs = make([]string, r.count(1))
	for i := range m.strs {
		m.strs[i] = r.str()
	}
	type indexed struct {
		id   taskID
		span span
	}
	list := make([]indexed, r.count(4))
	for i := range list {
		list[i] = indexed{taskID{m.str(r), r.str()}, span{r.num(), r.num()}}
	}
	if r.bad {
		return nil
	}
	m.entries, m.spans = r.s, make(map[taskID]span, len(list))
	for _, e := range list {
		if e.span.off+e.span.n > len(m.entries) {
			return nil
		}
		m.spans[e.id] = e.span
	}
	return m
}

func (m *memoStore) str(r *memoReader) string { return named(m.strs, r) }

// named is the entry of table an id read from r names, "" for an id past it, which marks r bad.
func named(table []string, r *memoReader) string {
	if id := r.id(len(table)); !r.bad {
		return table[id]
	}
	return ""
}

// entry decodes the task's entry, which is false when the store has none or its bytes are damaged.
func (m *memoStore) entry(id taskID) (taskEntry, bool) {
	at, ok := m.spans[id]
	if !ok {
		return taskEntry{}, false
	}
	r := &memoReader{s: m.entries[at.off : at.off+at.n]}
	e := taskEntry{kind: id.kind, key: id.key, inputs: r.sum()}
	for range r.count(2 + sha256.Size) {
		e.reads = append(e.reads, readDigest{m.str(r), m.str(r), r.sum()})
	}
	for range r.count(2 + sha256.Size) {
		q := question{oracle: named(m.oracles, r)}
		n := r.count(1)
		if n > 0 {
			q.args = make([]uint32, n)
		}
		last := 0
		for i := range q.args {
			last += r.num()
			if last >= len(m.args) {
				r.bad = true
				break
			}
			q.args[i] = uint32(last)
		}
		q.sum = r.sum()
		e.questions = append(e.questions, q)
	}
	for range r.count(4) {
		e.out = append(e.out, output{outputKind(m.str(r)), m.str(r), m.str(r), r.str()})
	}
	return e, !r.bad
}

// storeFile is where the full pass keeps its entries and a later one finds them.
type storeFile struct {
	path, key, binary string
	// load opens the store on the goroutine that started it, which a task waits for.
	load func() *memoStore
	// write is the goroutine that keeps the entries, which the last callback waits for.
	write sync.WaitGroup
}

// startLoad reads the last pass's store while the walk runs.
func (f *storeFile) startLoad() {
	f.load = sync.OnceValue(func() *memoStore { return openMemoStore(f.path, f.key, f.binary) })
	go f.load()
}

// startWrite keeps the entries beside the listings, replacing the last pass's store.
func (f *storeFile) startWrite(table questionArgs, entries []taskEntry) {
	f.write.Go(func() {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			log.Printf("typescript: memo store: %v", err)
			return
		}
		if err := writeAtomically(f.path, encodeMemoStore(f.key, f.binary, table, entries)); err != nil {
			log.Printf("typescript: memo store: %v", err)
		}
	})
}

// drop removes a store whose entries a run found wrong.
func (f *storeFile) drop() { os.Remove(f.path) }

func memoStorePath(cache, key string) string {
	return filepath.Join(cache, memoStoreName+"-"+key[:16])
}
