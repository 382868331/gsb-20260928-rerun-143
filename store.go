package gamesave

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

const (
	formatMagic   = "gamesave"
	formatVersion = 1
)

// CorruptSaveError wraps every failure that indicates the save bytes are not a
// complete, well-formed save. Callers can use errors.As to distinguish "half a
// file" from programming errors.
type CorruptSaveError struct{ reason string }

func (e *CorruptSaveError) Error() string { return "gamesave: corrupt save: " + e.reason }

func corrupt(format string, args ...any) error {
	return &CorruptSaveError{reason: fmt.Sprintf(format, args...)}
}

// IsCorrupt reports whether err means the save file is incomplete or invalid.
func IsCorrupt(err error) bool {
	var c *CorruptSaveError
	return errors.As(err, &c)
}

// Store is the save service. Identity (object pointer graph), migration
// (registry) and the write process (atomic temp-file + rename) are separate.
type Store struct {
	reg *Registry
}

func NewStore(reg *Registry) *Store { return &Store{reg: reg} }

type saveConfig struct {
	// writerWrap, when set, wraps the destination of the temporary file. It
	// exists to simulate a crash/full disk mid-write in tests.
	writerWrap func(io.Writer) io.Writer
}

type SaveOption func(*saveConfig)

// WithFaultyWriter simulates a write failure once exactly failAt bytes have
// been copied, proving the previously completed save is left untouched.
func WithFaultyWriter(failAt int) SaveOption {
	return func(c *saveConfig) {
		c.writerWrap = func(w io.Writer) io.Writer { return &faultWriter{w: w, failAt: failAt} }
	}
}

type faultWriter struct {
	w       io.Writer
	written int
	failAt  int
}

func (f *faultWriter) Write(p []byte) (int, error) {
	remaining := f.failAt - f.written
	if remaining <= 0 {
		return 0, fmt.Errorf("simulated write failure after %d bytes", f.written)
	}
	if len(p) > remaining {
		n, err := f.w.Write(p[:remaining])
		f.written += n
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("simulated write failure after %d bytes", f.written)
	}
	n, err := f.w.Write(p)
	f.written += n
	return n, err
}

// Save serializes every named root and all objects reachable from them, then
// commits atomically: bytes go to a sibling temp file which is fsynced and
// renamed over the destination. A crash at any point leaves the previous
// complete file in place.
func (s *Store) Save(path string, roots map[string]any, opts ...SaveOption) error {
	cfg := &saveConfig{}
	for _, o := range opts {
		o(cfg)
	}

	enc := newEncoder(s.reg)
	wireRoots := map[string]json.RawMessage{}
	for name, root := range roots {
		if root == nil {
			wireRoots[name] = json.RawMessage("null")
			continue
		}
		rv := reflect.ValueOf(root)
		if rv.Kind() != reflect.Ptr || rv.IsNil() {
			return fmt.Errorf("gamesave: root %q must be a non-nil pointer (object or struct)", name)
		}
		v := rv // keep the pointer value: intern keys on pointer identity
		val, err := enc.encodeRoot(v)
		if err != nil {
			return fmt.Errorf("gamesave: root %q: %w", name, err)
		}
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		wireRoots[name] = b
	}
	if err := enc.flush(); err != nil {
		return err
	}

	doc := wireDoc{
		Format:        formatMagic,
		FormatVersion: formatVersion,
		Objects:       enc.objects,
		Roots:         wireRoots,
	}
	var buf bytes.Buffer
	enc2 := json.NewEncoder(&buf)
	enc2.SetEscapeHTML(false)
	if err := enc2.Encode(&doc); err != nil {
		return err
	}

	return atomicWrite(path, buf.Bytes(), cfg)
}

func atomicWrite(path string, data []byte, cfg *saveConfig) (err error) {
	dir := filepath.Dir(path)
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, filepath.Base(path)+".tmp-"+hex.EncodeToString(randBytes[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	rolledBack := false
	rollback := func() {
		if rolledBack {
			return
		}
		rolledBack = true
		f.Close()
		os.Remove(tmp)
	}
	defer func() {
		if err != nil {
			rollback()
		}
	}()

	var w io.Writer = f
	if cfg.writerWrap != nil {
		w = cfg.writerWrap(w)
	}
	if _, err = w.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Replace the destination atomically. On POSIX rename is atomic, so readers
	// see either the old complete file or the new complete file, never half.
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	rolledBack = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Load reads a complete save and decodes the named roots into destinations.
// Each destinations value must be a pointer to the root variable. Loading is
// purely data decoding: unknown types fail, unknown code can never run.
func (s *Store) Load(path string, destinations map[string]any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var doc wireDocIn
	if err := dec.Decode(&doc); err != nil {
		return corrupt("cannot parse save: %v", err)
	}
	if doc.Format != formatMagic {
		return corrupt("not a gamesave file (format=%q)", doc.Format)
	}
	if doc.FormatVersion != formatVersion {
		return corrupt("unsupported format version %d (expected %d)", doc.FormatVersion, formatVersion)
	}
	if len(doc.Objects) == 0 && len(doc.Roots) == 0 {
		return corrupt("empty save")
	}
	ids := make([]int, len(doc.Objects))
	for i, o := range doc.Objects {
		ids[i] = o.ID
	}
	for i, id := range ids {
		if id != i {
			return corrupt("object table must be ordered by id starting at 0, got id %d at position %d", id, i)
		}
	}

	model := saveModel{objects: make([]savedObject, len(doc.Objects)), roots: doc.Roots}
	for i, o := range doc.Objects {
		fields, err := decodeFields(o.Fields)
		if err != nil {
			return corrupt("object %d (%s): %v", o.ID, o.Type, err)
		}
		model.objects[i] = savedObject{id: o.ID, typeOf: o.Type, version: o.Version, fields: fields}
	}
	d := newDecoder(s.reg, model)
	if err := d.validate(); err != nil {
		return err
	}
	if err := d.instantiate(); err != nil {
		return err
	}
	for name, dst := range destinations {
		rv := reflect.ValueOf(dst)
		if rv.Kind() != reflect.Ptr || rv.IsNil() {
			return fmt.Errorf("gamesave: destination for root %q must be a non-nil pointer", name)
		}
		if err := d.decodeRoot(name, rv); err != nil {
			return err
		}
	}
	// Roots are decoded (registering their ref slots) first, so root refs,
	// forward refs and cycles all resolve together.
	return d.resolve()
}
