// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

func TestATracedSurfaceRecordsEveryFrame(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(traceDirVariable, dir)
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	_, err := surface.Present(ttyapi.Frame{Rows: []string{"one"}})
	require.NoError(t, err)
	first := out.Len()
	_, err = surface.Present(ttyapi.Frame{Rows: []string{"two"}})
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(dir, "*.trace"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var records [][]byte
	for len(data) >= 8 {
		n := binary.LittleEndian.Uint64(data)
		records = append(records, data[8:8+n])
		data = data[8+n:]
	}
	require.Len(t, records, 2, "one record per frame")
	require.Equal(t, out.Bytes()[:first], records[0])
	require.Equal(t, out.Bytes()[first:], records[1])
}

func TestWithoutTheVariableNothingIsRecorded(t *testing.T) {
	t.Setenv(traceDirVariable, "")
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	_, isTrace := surface.out.(*traceWriter)
	require.False(t, isTrace)
}
