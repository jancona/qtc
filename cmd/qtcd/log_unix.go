//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// fileID is f's "device:inode", the form of JOURNAL_STREAM, or "" if f
// cannot be statted.
func fileID(f *os.File) string {
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
