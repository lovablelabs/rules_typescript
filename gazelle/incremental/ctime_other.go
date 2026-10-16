//go:build !linux && !darwin

package incremental

import (
	"io/fs"
	"time"
)

func ChangeTime(fs.FileInfo) time.Time { return time.Time{} }
