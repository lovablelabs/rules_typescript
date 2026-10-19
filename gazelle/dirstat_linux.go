//go:build amd64 || arm64

package typescript

import (
	"errors"
	"io/fs"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// statxMask is what incremental.StatLine and readDirLine read of an entry.
const statxMask = unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_SIZE | unix.STATX_MTIME | unix.STATX_CTIME | unix.STATX_INO

// lstat(2) does not trigger an automount; statx(2) does unless told not to.
var statx = func(fd int, name string, stx *unix.Statx_t) error {
	return unix.Statx(fd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT|unix.AT_STATX_SYNC_AS_STAT, statxMask, stx)
}

// statxMissing latches when the kernel or a seccomp filter refuses statx.
var statxMissing atomic.Bool

// dirStat stats a directory's entries relative to its descriptor, so the
// kernel resolves one name rather than the whole path.
type dirStat struct{ fd int }

// openDirStat is d's dirStat, valid while d stays open.
func openDirStat(d *os.File) dirStat {
	s := dirStat{fd: -1}
	if rc, err := d.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { s.fd = int(fd) })
	}
	return s
}

// lstat is entry.Info(), the path-based lstat it falls back to, read through statx.
func (s dirStat) lstat(entry fs.DirEntry) (fs.FileInfo, error) {
	if s.fd < 0 || statxMissing.Load() {
		return entry.Info()
	}
	var stx unix.Statx_t
	err := statx(s.fd, entry.Name(), &stx)
	for errors.Is(err, syscall.EINTR) {
		err = statx(s.fd, entry.Name(), &stx)
	}
	switch {
	case errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM):
		statxMissing.Store(true)
		return entry.Info()
	case err != nil:
		return nil, &fs.PathError{Op: "statx", Path: entry.Name(), Err: err}
	case stx.Mask&statxMask != statxMask:
		return entry.Info()
	}
	info := &statxInfo{name: entry.Name(), sys: syscall.Stat_t{
		Ino:  stx.Ino,
		Mode: uint32(stx.Mode),
		Size: int64(stx.Size),
		Mtim: syscall.Timespec{Sec: stx.Mtime.Sec, Nsec: int64(stx.Mtime.Nsec)},
		Ctim: syscall.Timespec{Sec: stx.Ctime.Sec, Nsec: int64(stx.Ctime.Nsec)},
	}}
	info.mode = fileMode(info.sys.Mode)
	return info, nil
}

// statxInfo is the fs.FileInfo os.Lstat returns, carrying the fields statxMask asks for.
type statxInfo struct {
	name string
	mode fs.FileMode
	sys  syscall.Stat_t
}

func (i *statxInfo) Name() string       { return i.name }
func (i *statxInfo) Size() int64        { return i.sys.Size }
func (i *statxInfo) Mode() fs.FileMode  { return i.mode }
func (i *statxInfo) ModTime() time.Time { return time.Unix(i.sys.Mtim.Unix()) }
func (i *statxInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *statxInfo) Sys() any           { return &i.sys }

// fileMode is os's conversion of a stat mode (os/stat_linux.go).
func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & syscall.S_IFMT {
	case syscall.S_IFBLK:
		mode |= fs.ModeDevice
	case syscall.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFDIR:
		mode |= fs.ModeDir
	case syscall.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case syscall.S_IFLNK:
		mode |= fs.ModeSymlink
	case syscall.S_IFSOCK:
		mode |= fs.ModeSocket
	}
	if m&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
