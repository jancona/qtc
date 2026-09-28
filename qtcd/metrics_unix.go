//go:build unix

package qtcd

import (
	"runtime"
	"syscall"
	"time"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// maxRSS returns the peak resident set size in bytes.
func maxRSS() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	rss := uint64(ru.Maxrss)
	if runtime.GOOS != "darwin" {
		rss <<= 10 // Linux reports kilobytes
	}
	return rss
}
