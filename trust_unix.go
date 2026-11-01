//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileUID reads the owning uid out of a FileInfo. Unix reports it in Sys(),
// and the type assertion can fail on a FileInfo that did not come from the
// filesystem (a test double, an archive entry), which is why it answers with
// ok rather than zero: uid 0 means ROOT, so a failed read must not spell
// itself "root-owned".
func fileUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}
