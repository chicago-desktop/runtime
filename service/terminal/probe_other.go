// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package terminal

import "time"

// waitReadable is unavailable here, so the terminal is never asked and the
// environment has the only word. Reporting this as a failed wait rather than
// a silent zero keeps the two cases apart in the one place that reads it.
func waitReadable(int, time.Duration) (bool, error) {
	return false, errProbeUnsupported
}
