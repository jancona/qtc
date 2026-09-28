//go:build !unix

package qtcd

import "time"

// cpuTime and maxRSS are not measured off Unix; the metrics line reports 0.
// qtcd runs on Linux; this lets the qtc CLI, which imports this package,
// build for Windows.
func cpuTime() time.Duration { return 0 }

func maxRSS() uint64 { return 0 }
