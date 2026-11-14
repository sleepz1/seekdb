//go:build amd64

package engine

import "time"

func init() {
	if hasAVX2 {
		ProbeDelay = 2 * time.Millisecond
	} else {
		ProbeDelay = 10 * time.Millisecond
	}
}
