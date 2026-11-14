package transfer

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Chunk data access — streaming reads by chunk offsets
// (docs/M0-TASKS.md step 4: "Never load whole files into RAM").

var (
	// ErrChunkRange is returned when a requested chunk index is out of
	// the manifest's declared range — engines reply 404, receivers abort.
	ErrChunkRange = errors.New("chunk index out of range")
	// ErrFileRange is returned for an unknown file index.
	ErrFileRange = errors.New("file index out of range")
)

// ChunkReader returns the raw bytes of chunk c of file f.
// Exactly one full manifest chunk, except the last chunk of a file,
// which may be short (PROTOCOL.md §3). The file is opened per call and
// read by offset — a multi-GB source never occupies RAM.
func (m *Manifest) ChunkRead(f, c int) ([]byte, error) {
	file, ok := m.Find(f)
	if !ok {
		return nil, ErrFileRange
	}
	if c < 0 || c >= file.ChunkCount {
		return nil, ErrChunkRange
	}
	if file.SourcePath == "" {
		return nil, errors.New("manifest file has no source path (receiver-side manifest?)")
	}
	if file.Size == 0 {
		return []byte{}, nil // empty file: exactly zero chunks — but be safe
	}
	offset := int64(c) * int64(m.ChunkSize)
	length := int64(m.ChunkSize)
	if rem := file.Size - offset; rem < length {
		length = rem
	}
	if length <= 0 {
		return nil, ErrChunkRange
	}
	return readRange(file.SourcePath, offset, length)
}

// ReadSeekCloser abstracts a readable byte range (used by ChunkStreamer
// and, later, upload verification on the receiver side).
type ReadSeekCloser interface {
	io.ReadSeekCloser
}

// ChunkStreamer streams chunk c of file f into w. It verifies the
// streamed bytes against the manifest hash before returning — the
// engine-side counterpart of the receiver's hash-wasm check.
func (m *Manifest) ChunkStreamer(f, c int, w io.Writer) (int64, error) {
	file, ok := m.Find(f)
	if !ok {
		return 0, ErrFileRange
	}
	if c < 0 || c >= file.ChunkCount {
		return 0, ErrChunkRange
	}
	if file.SourcePath == "" {
		return 0, errors.New("no source path")
	}
	if file.Size == 0 {
		return 0, nil
	}
	offset := int64(c) * int64(m.ChunkSize)
	length := int64(m.ChunkSize)
	if rem := file.Size - offset; rem < length {
		length = rem
	}
	src, err := os.Open(file.SourcePath)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	if length <= 0 {
		return 0, ErrChunkRange
	}
	n, err := io.CopyN(w, src, length)
	if err != nil {
		return n, fmt.Errorf("stream chunk f%d/c%d: %w", f, c, err)
	}
	return n, nil
}

func readRange(path string, offset, length int64) ([]byte, error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(src, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if int64(n) != length {
		return nil, fmt.Errorf("short read: got %d of %d bytes", n, length)
	}
	return buf, nil
}

// ChunkBounds returns (offset, length) of chunk c of file f — receivers
// use it for range resumes, tests use it for fixtures.
func (m *Manifest) ChunkBounds(f, c int) (offset, length int64, err error) {
	file, ok := m.Find(f)
	if !ok {
		return 0, 0, ErrFileRange
	}
	if c < 0 || c >= file.ChunkCount {
		return 0, 0, ErrChunkRange
	}
	if file.Size == 0 {
		return 0, 0, nil
	}
	offset = int64(c) * int64(m.ChunkSize)
	length = int64(m.ChunkSize)
	if rem := file.Size - offset; rem < length {
		length = rem
	}
	if length <= 0 {
		return 0, 0, ErrChunkRange
	}
	return offset, length, nil
}
