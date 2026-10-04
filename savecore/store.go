package savecore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// Physical save format (separate from the object-graph format):
//
//	"SVCR" (4 bytes magic) | uint32 LE payload length |
//	uint32 LE CRC32(IEEE) of payload | payload (graph JSON)
//
// A save is published by a single rename of a temp file, so a crash
// during Save leaves either the previous complete file or the new
// complete file, never a half-written one. The checksum catches
// bit rot and truncated/half-written payloads at read time.

const (
	formatName    = "savecore"
	formatVersion = 1
	magic         = "SVCR"
	headerLen     = 4 + 4 + 4
)

// CorruptionError means a save file failed structural or checksum
// validation; callers must not trust any partial state.
type CorruptionError struct{ reason string }

func (e *CorruptionError) Error() string {
	return "savecore: corrupted save file: " + e.reason
}

func corrupt(format string, args ...any) error {
	return &CorruptionError{reason: fmt.Sprintf(format, args...)}
}

// Store saves and loads one complete world snapshot at Path.
type Store struct {
	reg  *Registry
	Path string
}

func NewStore(reg *Registry, path string) *Store {
	return &Store{reg: reg, Path: path}
}

// TempPath is where Save writes before the atomic rename. It is
// exposed so tests (or crash drills) can simulate an interrupted save.
func (s *Store) TempPath() string {
	return s.Path + ".tmp"
}

// Save encodes root and publishes the file atomically.
func (s *Store) Save(root any) (err error) {
	payload, err := s.reg.Encode(root)
	if err != nil {
		return err
	}
	return WritePayload(s.Path, payload)
}

// WritePayload packs an already-encoded graph payload into the
// checksummed frame and publishes it at path atomically, with the
// same crash guarantees as Save. It exists for tooling (merges,
// migrations) that builds graph JSON without live objects.
func WritePayload(path string, payload []byte) (err error) {
	frame, err := pack(payload)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("savecore: create save dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".writing-*")
	if err != nil {
		return fmt.Errorf("savecore: create temp save: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmp != nil {
			tmp.Close()
		}
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(frame); err != nil {
		return fmt.Errorf("savecore: write temp save: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("savecore: flush temp save: %w", err)
	}
	if err = tmp.Close(); err != nil {
		tmp = nil
		return fmt.Errorf("savecore: close temp save: %w", err)
	}
	tmp = nil
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("savecore: publish save: %w", err)
	}
	if df, derr := os.Open(dir); derr == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

// Load reads and validates the save and reconstructs the world.
func (s *Store) Load() (any, error) {
	frame, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("savecore: read save: %w", err)
	}
	payload, err := unpack(frame)
	if err != nil {
		return nil, err
	}
	return s.reg.Decode(payload)
}

// Exists reports whether a save file is present.
func (s *Store) Exists() (bool, error) {
	switch _, err := os.Stat(s.Path); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func pack(payload []byte) ([]byte, error) {
	frame := make([]byte, headerLen+len(payload))
	copy(frame[0:4], magic)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(payload))
	copy(frame[headerLen:], payload)
	return frame, nil
}

// Pack wraps an encoded graph payload in the checksummed file frame.
// It is exported so tooling and tests can fabricate old-version files.
func Pack(payload []byte) ([]byte, error) { return pack(payload) }

// Unpack validates a file frame and returns its graph payload.
func Unpack(frame []byte) ([]byte, error) { return unpack(frame) }

func unpack(frame []byte) ([]byte, error) {
	if len(frame) < headerLen {
		return nil, corrupt("file shorter than header (%d bytes)", len(frame))
	}
	if string(frame[0:4]) != magic {
		return nil, corrupt("bad magic %q", string(frame[0:4]))
	}
	wantLen := binary.LittleEndian.Uint32(frame[4:8])
	wantCRC := binary.LittleEndian.Uint32(frame[8:12])
	payload := frame[headerLen:]
	if uint32(len(payload)) != wantLen {
		return nil, corrupt("length mismatch: header says %d bytes, file has %d", wantLen, len(payload))
	}
	if got := crc32.ChecksumIEEE(payload); got != wantCRC {
		return nil, corrupt("checksum mismatch: file %08x, header %08x", got, wantCRC)
	}
	if !json.Valid(payload) {
		return nil, corrupt("payload is not valid JSON")
	}
	return payload, nil
}
