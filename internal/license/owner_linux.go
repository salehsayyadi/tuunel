package license

import (
	"os"
	"syscall"
)

type owner struct{ uid, gid int }

func statOwner(st os.FileInfo) (owner, bool) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return owner{int(s.Uid), int(s.Gid)}, true
	}
	return owner{}, false
}
