//go:build amd64 || arm64

package typescript

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readDirLineBy reads dir's line through statx, or through the path-based lstat when byPath.
func readDirLineBy(t *testing.T, dir string, byPath bool) (string, time.Time) {
	t.Helper()
	t.Cleanup(func() { statxMissing.Store(false) })
	if byPath {
		statxMissing.Store(true)
		defer statxMissing.Store(false)
	}
	line, _, stamp, err := readDirLine(dir, []string{"BUILD.bazel"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return line, stamp
}

func countStatx(t *testing.T, refuse func(stx *unix.Statx_t, err error) error) *int {
	t.Helper()
	calls, real := new(int), statx
	statx = func(fd int, name string, stx *unix.Statx_t) error {
		*calls++
		return refuse(stx, real(fd, name, stx))
	}
	t.Cleanup(func() { statx = real })
	return calls
}

func TestReadDirLine_StatxReadsEachEntryAsLstatDoes(t *testing.T) {
	file := func(mode os.FileMode, mtime time.Time) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			writeFile(t, path, "export const x = 1;\n")
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if !mtime.IsZero() {
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	link := func(target string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		entry string
		make  func(t *testing.T, path string)
	}{
		{"regular file", "a.ts", file(0o644, time.Time{})},
		{"executable", "x.sh", file(0o755, time.Time{})},
		{"setuid, setgid and sticky", "s.ts", file(0o755|os.ModeSetuid|os.ModeSetgid|os.ModeSticky, time.Time{})},
		{"mtime with nanoseconds", "n.ts", file(0o644, time.Unix(1_700_000_000, 123_456_789))},
		{"mtime before the epoch", "old.ts", file(0o644, time.Unix(-1, 500_000_000))},
		{"mtime past 2262", "far.ts", file(0o644, time.Unix(9_300_000_000, 1))},
		{"BUILD file", "BUILD.bazel", file(0o644, time.Time{})},
		{"link to a file", "l.ts", link("target.ts")},
		{"dangling link", "d.ts", link("missing.ts")},
		{"link to a directory", "ld", link("tdir")},
		{"fifo", "p", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"socket", "s.sock", func(t *testing.T, path string) {
			if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "target.ts"), "export {};\n")
			if err := os.Mkdir(filepath.Join(dir, "tdir"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.make(t, filepath.Join(dir, tc.entry))
			calls := countStatx(t, func(_ *unix.Statx_t, err error) error { return err })
			byPath, byPathStamp := readDirLineBy(t, dir, true)
			got, stamp := readDirLineBy(t, dir, false)
			if *calls == 0 || statxMissing.Load() {
				t.Fatalf("statx was called %d times and is missing: %t; the comparison is vacuous", *calls, statxMissing.Load())
			}
			if got != byPath || !stamp.Equal(byPathStamp) {
				t.Errorf("statx reads\n%s(stamp %v); lstat reads\n%s(stamp %v)", got, stamp, byPath, byPathStamp)
			}
		})
	}
}

func TestReadDirLine_FallsBackToLstatWhereStatxFallsShort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refuse    func(stx *unix.Statx_t, err error) error
		wantCalls int
	}{
		{"no statx in the kernel", func(*unix.Statx_t, error) error { return syscall.ENOSYS }, 1},
		{"statx denied by seccomp", func(*unix.Statx_t, error) error { return syscall.EPERM }, 1},
		{"no ctime from the file system", func(stx *unix.Statx_t, err error) error {
			stx.Mask &^= unix.STATX_CTIME
			stx.Ctime.Sec, stx.Ctime.Nsec = 0, 0
			return err
		}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"a.ts", "b.ts", "c.ts"} {
				writeFile(t, filepath.Join(dir, name), "export {};\n")
			}
			want, _ := readDirLineBy(t, dir, true)
			calls := countStatx(t, tc.refuse)
			for range 2 {
				if got, _ := readDirLineBy(t, dir, false); got != want {
					t.Errorf("the fallback reads\n%s; lstat reads\n%s", got, want)
				}
			}
			if *calls != tc.wantCalls {
				t.Errorf("statx was called %d times over two reads of three entries; want %d", *calls, tc.wantCalls)
			}
		})
	}
}

func TestRunRecord_TheReadPassClosesEveryDirectoryItOpens(t *testing.T) {
	root := t.TempDir()
	rec := &runRecord{}
	for i := range 256 {
		dir := filepath.Join(root, "d", string(rune('a'+i%26))+string(rune('a'+i/26)))
		writeFile(t, filepath.Join(dir, "f.ts"), "export {};\n")
		rec.dirs = append(rec.dirs, dir)
	}
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := fds()
	if _, err := rec.read([]string{"BUILD.bazel"}, "", nil); err != nil {
		t.Fatal(err)
	}
	if after := fds(); after != before {
		t.Errorf("%d descriptors open after the pass over %d directories, %d before it", after, len(rec.dirs), before)
	}
}
