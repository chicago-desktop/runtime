// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// A record of what a surface wrote, for replaying it through a terminal
// emulator outside the running application.
//
// WIPPY_TTY_TRACE_DIR names a directory on the machine the runtime runs on;
// every surface opened while it is set writes one file there. The variable is
// read from the runtime's own environment, never from an SSH client's: a
// client must not choose where the server writes.
//
// Each write is one record: eight bytes of little-endian length, then the
// bytes as the terminal got them. A surface writes a frame in one call, so a
// record is a frame, and a replay can look at the terminal between frames.

const traceDirVariable = "WIPPY_TTY_TRACE_DIR"

var traceSeq atomic.Uint64

type traceWriter struct {
	out  io.Writer
	file *os.File
	mu   sync.Mutex
	dead bool
}

// traced wraps out in a recorder when the trace directory is set. A failure
// to open the file leaves out as it was: a trace is never worth a desktop.
func traced(out io.Writer) io.Writer {
	dir := os.Getenv(traceDirVariable)
	if dir == "" {
		return out
	}
	name := fmt.Sprintf("surface-%s-%03d.trace", time.Now().Format("20060102-150405"), traceSeq.Add(1))
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return out
	}
	return &traceWriter{out: out, file: file}
}

func (t *traceWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	if !t.dead {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(p)))
		if _, err := t.file.Write(size[:]); err == nil {
			_, err = t.file.Write(p)
			t.dead = err != nil
		} else {
			t.dead = true
		}
	}
	t.mu.Unlock()
	return t.out.Write(p)
}

func (t *traceWriter) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dead = true
	return t.file.Close()
}

// closeTrace ends the record of a surface that has one.
func closeTrace(out io.Writer) {
	if t, ok := out.(*traceWriter); ok {
		_ = t.Close()
	}
}
