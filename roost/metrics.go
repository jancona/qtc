package roost

import (
	"runtime"
	"syscall"
	"time"
)

// runMetrics logs process resource use at the configured interval so spike
// results can be recorded: goroutines, heap, max RSS, and CPU fraction since
// the previous sample.
func (r *Roost) runMetrics() {
	t := time.NewTicker(r.cfg.MetricsInterval)
	defer t.Stop()
	lastCPU, lastWall := cpuTime(), time.Now()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		cpu, wall := cpuTime(), time.Now()
		frac := float64(cpu-lastCPU) / float64(wall.Sub(lastWall))
		lastCPU, lastWall = cpu, wall
		r.log.Info("metrics",
			"goroutines", runtime.NumGoroutine(),
			"heap_mb", ms.HeapAlloc>>20,
			"sys_mb", ms.Sys>>20,
			"maxrss_mb", maxRSS()>>20,
			"cpu_pct", int(frac*100),
			"peers", len(r.host.Network().Peers()),
		)
	}
}

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
