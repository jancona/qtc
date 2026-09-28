package qtcd

import (
	"runtime"
	"time"
)

// runMetrics logs process resource use at the configured interval so spike
// results can be recorded: goroutines, heap, max RSS, and CPU fraction since
// the previous sample.
func (r *Station) runMetrics() {
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
