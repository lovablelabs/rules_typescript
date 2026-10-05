// Package incremental fingerprints the inputs of a Gazelle run, so a driver can
// skip a run whose inputs equal those of an earlier run that changed nothing.
package incremental

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Options names what a run reads: Root walked, Skip pruned by relative path,
// Extra stat'ed one by one (absence included), Key for the arguments.
type Options struct {
	Root  string
	Skip  func(rel string) bool
	Extra []string
	Key   []string
	// ByContent selects files fingerprinted by their bytes rather than their stat: a
	// pipeline that rewrites a file with the same bytes still reached a fixed point.
	ByContent func(rel string) bool
}

// Digest is the fingerprint of every entry Entries returns.
func Digest(o Options) (string, error) {
	entries, err := Entries(o)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%q %s\n", k, entries[k])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Entries maps every path under Root, each Extra and each Key to its fingerprint
// line: stat for a file (bytes under ByContent), target for a symlink.
func Entries(o Options) (map[string]string, error) {
	out := map[string]string{}
	var mu sync.Mutex
	add := func(k, v string) {
		mu.Lock()
		out[k] = v
		mu.Unlock()
	}
	sem := make(chan struct{}, 4*runtime.GOMAXPROCS(0))
	if err := walkEntries(o, "", sem, add); err != nil {
		return nil, err
	}
	for _, path := range o.Extra {
		out["extra:"+path] = entryLine(path)
	}
	for i, k := range o.Key {
		out[fmt.Sprintf("key:%d", i)] = k
	}
	return out, nil
}

func walkEntries(o Options, rel string, sem chan struct{}, add func(k, v string)) error {
	dir := filepath.Join(o.Root, filepath.FromSlash(rel))
	sem <- struct{}{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		<-sem
		return err
	}
	var children []string
	for _, entry := range entries {
		childRel := entry.Name()
		if rel != "" {
			childRel = rel + "/" + entry.Name()
		}
		switch {
		case o.Skip != nil && o.Skip(childRel):
			add(childRel, "skip")
		case entry.IsDir():
			add(childRel, "dir")
			children = append(children, childRel)
		default:
			info, err := entry.Info()
			if err != nil {
				add(childRel, "gone")
				continue
			}
			line := statLine(filepath.Join(dir, entry.Name()), info)
			if o.ByContent != nil && info.Mode().IsRegular() && o.ByContent(childRel) {
				if data, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
					line = fmt.Sprintf("%o %x", info.Mode(), sha256.Sum256(data))
				}
			}
			add(childRel, line)
		}
	}
	<-sem
	var wg sync.WaitGroup
	errs := make([]error, len(children))
	for i, child := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = walkEntries(o, child, sem, add)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func entryLine(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return "absent"
	}
	return statLine(path, info)
}

func statLine(path string, info fs.FileInfo) string {
	var ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	mtime := info.ModTime().UnixNano()
	line := fmt.Sprintf("%o %d %d %d", info.Mode(), info.Size(), mtime, ino)
	if info.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		line += " -> " + target
	}
	return line
}

// Load returns the digest a previous run saved at path, or "" when there is none.
func Load(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Save records digest at path atomically.
func Save(path, digest string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(digest+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StatCache memoizes stat lines for the life of one run, where many
// fingerprints share most of their paths.
type StatCache struct{ lines sync.Map }

// Line is path's fingerprint line, "absent" when it does not exist.
func (c *StatCache) Line(path string) string { return c.line(path) }

func (c *StatCache) line(path string) string {
	if c == nil {
		return entryLine(path)
	}
	if line, ok := c.lines.Load(path); ok {
		return line.(string)
	}
	line := entryLine(path)
	c.lines.Store(path, line)
	return line
}

// StatDigest fingerprints paths by their stat, an absent path included.
func StatDigest(paths []string) string {
	return (*StatCache)(nil).Digest(paths)
}

func (c *StatCache) Digest(paths []string) string {
	lines := make([]string, len(paths))
	var wg sync.WaitGroup
	chunk := (len(paths) + runtime.GOMAXPROCS(0) - 1) / max(1, runtime.GOMAXPROCS(0))
	for start := 0; start < len(paths); start += max(1, chunk) {
		end := min(len(paths), start+max(1, chunk))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := start; i < end; i++ {
				lines[i] = paths[i] + " " + c.line(paths[i])
			}
		}()
	}
	wg.Wait()
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// NameTree digests the names, not the contents, below a directory: what a glob
// can match. Digests are memoized for the life of the tree, one run.
type NameTree struct {
	Skip func(name string) bool
	// File, when set, says which file names count; directories always do.
	File func(name string) bool
	mu   sync.Mutex
	memo map[string]string
}

func (t *NameTree) Digest(dir string) string {
	dir = filepath.Clean(dir)
	t.mu.Lock()
	if d, ok := t.memo[dir]; ok {
		t.mu.Unlock()
		return d
	}
	t.mu.Unlock()
	entries, err := os.ReadDir(dir)
	var lines []string
	if err != nil {
		lines = []string{"absent"}
	}
	for _, entry := range entries {
		switch {
		case t.Skip != nil && t.Skip(entry.Name()):
		case entry.Type()&fs.ModeSymlink != 0:
			lines = append(lines, "l "+entry.Name())
		case entry.IsDir():
			lines = append(lines, "d "+entry.Name()+" "+t.Digest(filepath.Join(dir, entry.Name())))
		case t.File == nil || t.File(entry.Name()):
			lines = append(lines, "f "+entry.Name())
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	d := hex.EncodeToString(sum[:])
	t.mu.Lock()
	if t.memo == nil {
		t.memo = map[string]string{}
	}
	t.memo[dir] = d
	t.mu.Unlock()
	return d
}
