//go:build !linux && !darwin

package typescript

import (
	"io/fs"
	"time"
)

func changeTime(fs.FileInfo) time.Time { return time.Time{} }
