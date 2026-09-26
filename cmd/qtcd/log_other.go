//go:build !unix

package main

import "os"

// fileID is "" where there is no journal to match.
func fileID(*os.File) string { return "" }
