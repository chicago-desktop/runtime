package migration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const RawFormat = "legacy-source-v1"
const MaterializedFormat = "legacy-materialized-v1"

type Manifest struct {
	Digest  []byte `json:"digest"`
	Head    uint64 `json:"head"`
	Maximum uint64 `json:"maximum"`
	Count   uint64 `json:"count"`
}

type Header struct {
	BaselineDigest []byte   `json:"baseline_digest"`
	Baseline       []byte   `json:"baseline"`
	Format         string   `json:"format"`
	Manifest       Manifest `json:"manifest"`
}

type Record struct {
	ResolutionDigest *string `json:"resolution_digest"`
	Changes          []byte  `json:"changes"`
	Resolution       []byte  `json:"resolution"`
	Snapshot         []byte  `json:"snapshot,omitempty"`
	Revision         uint64  `json:"revision"`
	Parent           uint64  `json:"parent"`
}

type sequence struct {
	seen      map[uint64]struct{}
	count     uint64
	last      uint64
	foundHead bool
}

func (s *sequence) accept(header Header, record *Record) error {
	if record == nil || s.count >= header.Manifest.Count {
		return errors.New("unexpected history record")
	}
	if header.Format == RawFormat && len(record.Snapshot) != 0 {
		return errors.New("raw history contains a snapshot")
	}
	if s.count == 0 {
		if record.Revision != 0 || record.Parent != 0 {
			return errors.New("history must start at root zero")
		}
		s.seen = make(map[uint64]struct{})
	} else {
		if record.Revision <= s.last {
			return errors.New("history revisions are not ordered")
		}
		if _, exists := s.seen[record.Parent]; !exists {
			return errors.New("history parent is missing")
		}
	}
	if record.Revision > header.Manifest.Maximum {
		return errors.New("history revision exceeds the source maximum")
	}
	s.seen[record.Revision] = struct{}{}
	s.count++
	s.last = record.Revision
	s.foundHead = s.foundHead || record.Revision == header.Manifest.Head
	return nil
}

func (s *sequence) finish(header Header) error {
	if s.count != header.Manifest.Count || s.last != header.Manifest.Maximum || !s.foundHead {
		return errors.New("history records do not match the source manifest")
	}
	return nil
}

func validateHeader(header Header, limit int) error {
	if limit <= 0 {
		return errors.New("maximum history record bytes must be positive")
	}
	if header.Format != RawFormat && header.Format != MaterializedFormat {
		return errors.New("unsupported history bundle format")
	}
	if header.Manifest.Count == 0 || header.Manifest.Head > header.Manifest.Maximum {
		return errors.New("invalid source history manifest")
	}
	return nil
}

type Reader struct {
	reader   bufio.Scanner
	sequence sequence
	header   Header
	ended    bool
}

func NewReader(reader io.Reader, maximumRecordBytes int) (*Reader, error) {
	if reader == nil || maximumRecordBytes <= 0 {
		return nil, errors.New("history reader and positive record limit are required")
	}
	result := &Reader{reader: *bufio.NewScanner(reader)}
	result.reader.Buffer(nil, maximumRecordBytes)
	line, err := result.line()
	if err != nil {
		return nil, fmt.Errorf("read history header: %w", err)
	}
	if err := decode(line, &result.header); err != nil {
		return nil, err
	}
	if err := validateHeader(result.header, maximumRecordBytes); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Reader) Header() Header { return r.header }

func (r *Reader) Next() (*Record, error) {
	if r.ended {
		return nil, io.EOF
	}
	line, err := r.line()
	if errors.Is(err, io.EOF) {
		if err := r.sequence.finish(r.header); err != nil {
			return nil, err
		}
		r.ended = true
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}
	var record Record
	if err := decode(line, &record); err != nil {
		return nil, err
	}
	if err := r.sequence.accept(r.header, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *Reader) line() ([]byte, error) {
	if r.reader.Scan() {
		return r.reader.Bytes(), nil
	}
	if err := r.reader.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("history line must contain one JSON value")
	}
	return nil
}

type Writer struct {
	writer             io.Writer
	failure            error
	sequence           sequence
	header             Header
	maximumRecordBytes int
	closed             bool
}

func NewWriter(writer io.Writer, header Header, maximumRecordBytes int) (*Writer, error) {
	if writer == nil {
		return nil, errors.New("history writer is required")
	}
	if err := validateHeader(header, maximumRecordBytes); err != nil {
		return nil, err
	}
	result := &Writer{writer: writer, header: header, maximumRecordBytes: maximumRecordBytes}
	if err := result.write(header); err != nil {
		return nil, err
	}
	return result, nil
}

func (w *Writer) Write(record *Record) error {
	if w.failure != nil {
		return w.failure
	}
	if w.closed {
		return errors.New("history writer is closed")
	}
	data, err := json.Marshal(record)
	if err == nil && len(data)+1 > w.maximumRecordBytes {
		err = errors.New("history record exceeds the byte limit")
	}
	if err == nil {
		err = w.sequence.accept(w.header, record)
	}
	if err == nil {
		err = w.writeBytes(data)
	}
	w.failure = err
	return err
}

func (w *Writer) Close() error {
	if w.failure != nil {
		return w.failure
	}
	w.closed = true
	return w.sequence.finish(w.header)
}

func (w *Writer) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data)+1 > w.maximumRecordBytes {
		return errors.New("history record exceeds the byte limit")
	}
	return w.writeBytes(data)
}

func (w *Writer) writeBytes(data []byte) error {
	data = append(data, '\n')
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
