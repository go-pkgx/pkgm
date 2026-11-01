//go:build !unix

package main

import "io/fs"

// fileUID has no answer off unix. Windows has ACLs rather than a uid and
// wasip1 reports neither, so the honest return is "not known" and the mode
// carries whatever judgement is left — see ownerOf, which turns this into
// errNoOwner rather than letting a zero read as root.
func fileUID(fs.FileInfo) (uint32, bool) { return 0, false }
