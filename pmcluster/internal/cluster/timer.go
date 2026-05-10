package cluster

import "time"

// newTimer is a tiny indirection over time.NewTimer so down.go's settle
// wait stays trivially testable (a future test can swap newTimer out).
func newTimer(seconds int) *time.Timer {
	return time.NewTimer(time.Duration(seconds) * time.Second)
}
