//go:build !linux

package license

import "os"

type owner struct{ uid, gid int }

func statOwner(os.FileInfo) (owner, bool) { return owner{}, false }
