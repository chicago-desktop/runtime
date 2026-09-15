// SPDX-License-Identifier: MPL-2.0

//go:build unix

package terminal

import (
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable waits for the descriptor to have something to read.
//
// poll rather than a read deadline: os.Stdin is a blocking descriptor and
// answers SetReadDeadline with "file type does not support deadline", so the
// only other way to bound a read is a goroutine blocked in Read — and that
// goroutine keeps the descriptor busy after the caller has given up, eating
// the first key the person presses.
func waitReadable(fd int, timeout time.Duration) (bool, error) {
	milliseconds := int(timeout.Milliseconds())
	if milliseconds <= 0 {
		milliseconds = 1
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, milliseconds)
		if err == unix.EINTR {
			// A signal woke the wait. The terminal is still going to answer,
			// and giving up here would report "no graphics" for a terminal
			// that has them.
			continue
		}
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
}
