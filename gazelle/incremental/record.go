package incremental

import (
	"bytes"
	"encoding/gob"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Record is the input state of a run that changed nothing, with the signature
// of every source in it.
type Record struct {
	Entries    map[string]string
	Signatures map[string]string
}

// SignatureSource is a file whose edits Signature can judge. A config's
// literals reach generated rules directly, so a config is never one.
func SignatureSource(rel string) bool {
	if strings.Contains(rel, ":") || strings.Contains(path.Base(rel), ".config.") {
		return false
	}
	switch path.Ext(rel) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// NewRecord signs every source among entries.
func NewRecord(root string, entries map[string]string) *Record {
	var sources []string
	for rel := range entries {
		if SignatureSource(rel) {
			sources = append(sources, rel)
		}
	}
	sigs := make([]string, len(sources))
	var wg sync.WaitGroup
	step := max(1, (len(sources)+runtime.GOMAXPROCS(0)-1)/runtime.GOMAXPROCS(0))
	for start := 0; start < len(sources); start += step {
		end := min(len(sources), start+step)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := start; i < end; i++ {
				sigs[i] = signFile(root, sources[i])
			}
		}()
	}
	wg.Wait()
	r := &Record{Entries: entries, Signatures: map[string]string{}}
	for i, rel := range sources {
		if sigs[i] != "" {
			r.Signatures[rel] = sigs[i]
		}
	}
	return r
}

func signFile(root, rel string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	sig, err := Signature(rel, data)
	if err != nil {
		return ""
	}
	return sig
}

// Unchanged reports whether now differs from the record only in sources whose
// signatures it kept: no path appeared or vanished, and nothing else moved.
func (r *Record) Unchanged(root string, now map[string]string) bool {
	if r == nil || len(now) != len(r.Entries) {
		return false
	}
	for rel, line := range now {
		was, ok := r.Entries[rel]
		if !ok {
			return false
		}
		if was == line {
			continue
		}
		sig, ok := r.Signatures[rel]
		if !ok || signFile(root, rel) != sig {
			return false
		}
	}
	return true
}

func LoadRecord(file string) *Record {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var r Record
	if gob.NewDecoder(bytes.NewReader(data)).Decode(&r) != nil {
		return nil
	}
	return &r
}

// Save writes the record atomically.
func (r *Record) Save(file string) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(r); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// Changed is the paths whose entries moved since the record, when nothing
// appeared or vanished; ok is false otherwise.
func (r *Record) Changed(now map[string]string) (paths []string, ok bool) {
	if r == nil || len(now) != len(r.Entries) {
		return nil, false
	}
	for rel, line := range now {
		was, found := r.Entries[rel]
		if !found {
			return nil, false
		}
		if was != line {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	return paths, true
}
