//go:build !linux || !(amd64 || arm64)

package typescript

import (
	"io/fs"
	"os"
)

type dirStat struct{}

func openDirStat(*os.File) dirStat { return dirStat{} }

func (dirStat) lstat(entry fs.DirEntry) (fs.FileInfo, error) { return entry.Info() }
