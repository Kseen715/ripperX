//go:build !unix

package main

// Not knowing how much room is left is an answer the rest of ripperX
// already handles: the page leaves the figure out, and the check in front
// of a rip lets it through. The drive code is Linux-only in any case, so
// nothing here is ever reached - it exists so the package still builds
// where statfs does not.
func diskSpace(string) (free, total int64, ok bool) { return 0, 0, false }
