package incremental

import (
	"io/fs"
	"syscall"
	"time"
)

func ChangeTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Unix())
	}
	return time.Time{}
}
