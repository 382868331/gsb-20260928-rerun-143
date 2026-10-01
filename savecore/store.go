package savecore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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
	reg           *Registry
	Path          string
	partitionFor  Partitioner
	BeforePublish func(generationDir string) error
}

func NewStore(reg *Registry, path string) *Store {
	return &Store{reg: reg, Path: path}
}

// Save encodes root and publishes a complete snapshot. Partitioned stores
// publish a new generation only after every partition is durable and the
// entire cross-partition graph has validated.
func (s *Store) Save(root any) error {
	if s.partitionFor != nil {
		return s.savePartitioned(root)
	}
	return s.saveSingle(root)
}

// TempPath is where Save writes before the atomic rename. It is
// exposed so tests (or crash drills) can simulate an interrupted save.
func (s *Store) TempPath() string {
	return s.Path + ".tmp"
}

func (s *Store) saveSingle(root any) (err error) {
	payload, err := s.reg.Encode(root)
	if err != nil {
		return err
	}
	frame, err := pack(payload)
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("savecore: create save dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.Path)+".writing-*")
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
	if err = os.Rename(tmpName, s.Path); err != nil {
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
	if s.partitionFor != nil {
		loader, err := s.OpenLoader()
		if err != nil {
			return nil, err
		}
		return loader.LoadAll()
	}
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

const currentGenName = "current.json"

type currentPointer struct {
	Generation string `json:"generation"`
}

func (s *Store) savePartitioned(root any) (err error) {
	data, manifestRaw, partitionFiles, err := s.reg.encodePartitioned(root, s.partitionFor)
	if err != nil {
		return err
	}
	if err := s.validateGraph(data, manifestRaw); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Path, 0o755); err != nil {
		return fmt.Errorf("savecore: create save dir: %w", err)
	}
	genID := newGenerationID()
	genDir := filepath.Join(s.Path, genID)
	if err := os.Mkdir(genDir, 0o755); err != nil {
		return fmt.Errorf("savecore: create generation: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(genDir)
		}
	}()
	for _, pf := range partitionFiles {
		raw, err := json.Marshal(pf)
		if err != nil {
			return err
		}
		frame, err := pack(raw)
		if err != nil {
			return err
		}
		if err := writeFileSync(filepath.Join(genDir, safePartitionFile(pf.Partition)), frame); err != nil {
			return err
		}
	}
	if err := writeFileSync(filepath.Join(genDir, "manifest.json"), manifestRaw); err != nil {
		return err
	}
	if s.BeforePublish != nil {
		if err := s.BeforePublish(genDir); err != nil {
			return err
		}
	}
	pointerRaw, err := json.Marshal(currentPointer{Generation: genID})
	if err != nil {
		return err
	}
	tmpPointer := filepath.Join(s.Path, "current.json.tmp")
	if err := writeFileSync(tmpPointer, pointerRaw); err != nil {
		return err
	}
	if err := os.Rename(tmpPointer, filepath.Join(s.Path, currentGenName)); err != nil {
		return fmt.Errorf("savecore: publish generation pointer: %w", err)
	}
	cleanup = false
	if df, derr := os.Open(s.Path); derr == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

func writeFileSync(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".writing-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func safePartitionFile(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_")
	return "partition-" + replacer.Replace(name) + ".sv"
}

func (s *Store) validateGraph(data *graphData, manifestRaw []byte) error {
	var man partitionManifest
	if err := json.Unmarshal(manifestRaw, &man); err != nil {
		return err
	}
	d := &decoder{reg: s.reg, table: map[int]reflect.Value{}, filled: map[int]bool{}}
	for _, n := range man.Nodes {
		if _, err := d.placeholder(n.Type, n.ID); err != nil {
			return err
		}
	}
	for _, id := range sortedNodeIDs(data.Nodes) {
		raw, err := json.Marshal(data.Nodes[id])
		if err != nil {
			return err
		}
		if _, err := d.decodeRef(raw); err != nil {
			return err
		}
	}
	if _, ok := d.table[data.RootID]; !ok {
		return fmt.Errorf("savecore: manifest root $ref:%d is missing", data.RootID)
	}
	return nil
}

func sortedNodeIDs(nodes map[int]map[string]interface{}) []int {
	ids := make([]int, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// Loader is one on-demand loading session. All references share its identity
// table, so loading partitions in any order preserves pointer identity.
type Loader struct {
	eager    any
	store    *Store
	genDir   string
	manifest partitionManifest
	decoder  *decoder
	loaded   map[string]bool
}

func (s *Store) OpenLoader() (*Loader, error) {
	if s.partitionFor == nil {
		return nil, fmt.Errorf("savecore: OpenLoader requires a partitioned store")
	}
	pointerRaw, err := os.ReadFile(filepath.Join(s.Path, currentGenName))
	if err != nil {
		return nil, fmt.Errorf("savecore: read current generation: %w", err)
	}
	var pointer currentPointer
	if err := json.Unmarshal(pointerRaw, &pointer); err != nil {
		return nil, corrupt("generation pointer is not valid JSON: %v", err)
	}
	if pointer.Generation == "" || filepath.Base(pointer.Generation) != pointer.Generation {
		return nil, corrupt("invalid generation name %q", pointer.Generation)
	}
	genDir := filepath.Join(s.Path, pointer.Generation)
	manifestRaw, err := os.ReadFile(filepath.Join(genDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("savecore: read manifest: %w", err)
	}
	var man partitionManifest
	if err := json.Unmarshal(manifestRaw, &man); err != nil {
		return nil, corrupt("manifest is not valid JSON: %v", err)
	}
	if man.Format != formatName || man.FormatVer != partitionFormatVersion {
		return nil, corrupt("unsupported partitioned format %s/%d", man.Format, man.FormatVer)
	}
	d := &decoder{reg: s.reg, table: map[int]reflect.Value{}, filled: map[int]bool{}}
	if err := validateManifestAndPlaceholders(man, d, s.reg); err != nil {
		return nil, err
	}
	return &Loader{store: s, genDir: genDir, manifest: man, decoder: d, loaded: map[string]bool{}}, nil
}

func (l *Loader) LoadAll() (any, error) {
	if l.eager != nil {
		return l.eager, nil
	}
	names := make([]string, 0, len(l.manifest.Partitions))
	for _, p := range l.manifest.Partitions {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := l.LoadPartition(name); err != nil {
			return nil, err
		}
	}
	return l.Root(), nil
}

func (l *Loader) Root() any {
	v, ok := l.decoder.table[l.manifest.Root.Ref]
	if !ok {
		return nil
	}
	return v.Interface()
}

func (l *Loader) LoadPartition(name string) error {
	if l.eager != nil {
		return nil
	}
	if l.loaded[name] {
		return nil
	}
	info, err := l.partitionInfo(name)
	if err != nil {
		return err
	}
	frame, err := os.ReadFile(filepath.Join(l.genDir, safePartitionFile(name)))
	if err != nil {
		return l.partitionFailure(info, err.Error())
	}
	raw, err := unpack(frame)
	if err != nil {
		return l.partitionFailure(info, err.Error())
	}
	var pf partitionFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return l.partitionFailure(info, "partition is not valid JSON: "+err.Error())
	}
	if pf.Format != formatName || pf.FormatVer != partitionFormatVersion || pf.Partition != name {
		return l.partitionFailure(info, "partition header does not match manifest")
	}
	if crc32.ChecksumIEEE(raw) != info.CRC {
		return l.partitionFailure(info, "checksum does not match manifest")
	}
	if len(pf.Nodes) != len(info.Nodes) {
		return l.partitionFailure(info, "node count does not match manifest")
	}
	known := map[int]bool{}
	for _, n := range l.manifest.Nodes {
		known[n.ID] = true
	}
	if err := validatePartitionReferences(pf.Nodes, known); err != nil {
		return l.partitionFailure(info, err.Error())
	}
	for i, node := range pf.Nodes {
		idValue, ok := node[keyID].(float64)
		if !ok || int(idValue) != info.Nodes[i] {
			return l.partitionFailure(info, "node id does not match manifest")
		}
		id := int(idValue)
		typeName, _ := node[keyType].(string)
		verValue, verOK := node[keyVer].(float64)
		want, ok := l.nodeMeta(id)
		if !ok || want.Partition != name || !verOK || typeName != want.Type || int(verValue) != want.Ver {
			return l.partitionFailure(info, "node metadata does not match manifest")
		}
		rawNode, err := json.Marshal(node)
		if err != nil {
			return l.partitionFailure(info, err.Error())
		}
		if _, err := l.decoder.decodeRef(rawNode); err != nil {
			return err
		}
	}
	l.loaded[name] = true
	return nil
}

func (l *Loader) nodeMeta(id int) (partitionNodeRef, bool) {
	for _, n := range l.manifest.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return partitionNodeRef{}, false
}

func (l *Loader) partitionInfo(name string) (partitionInfo, error) {
	for _, p := range l.manifest.Partitions {
		if p.Name == name {
			return p, nil
		}
	}
	return partitionInfo{}, fmt.Errorf("savecore: unknown partition %q", name)
}

func (l *Loader) partitionFailure(info partitionInfo, reason string) error {
	types := make([]string, 0, len(info.Nodes))
	byID := map[int]string{}
	for _, n := range l.manifest.Nodes {
		byID[n.ID] = n.Type
	}
	for _, id := range info.Nodes {
		types = append(types, byID[id])
	}
	return &PartitionCorruptionError{
		Partition:   info.Name,
		ObjectIDs:   append([]int(nil), info.Nodes...),
		ObjectTypes: types,
		Reason:      reason,
	}
}

// PartitionNames returns all partition names in the open snapshot.
func (l *Loader) PartitionNames() []string {
	if l.eager != nil {
		return nil
	}
	names := make([]string, 0, len(l.manifest.Partitions))
	for _, p := range l.manifest.Partitions {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// Object returns the stable placeholder or loaded object for a graph id.
func (l *Loader) Object(id int) (any, bool) {
	if l.decoder == nil {
		return nil, false
	}
	v, ok := l.decoder.table[id]
	if !ok {
		return nil, false
	}
	return v.Interface(), true
}

// IsLoaded reports whether a partition's fields have been populated.
func (l *Loader) IsLoaded(name string) bool { return l.loaded[name] }

// Upgrade loads and migrates every partition and only then publishes a new
// generation. Corrupt or internally inconsistent old saves leave the current
// pointer untouched.
func (s *Store) Upgrade() error {
	loader, err := s.OpenLoader()
	if err != nil {
		return err
	}
	root, err := loader.LoadAll()
	if err != nil {
		return err
	}
	return s.Save(root)
}

func validateManifestAndPlaceholders(man partitionManifest, d *decoder, reg *Registry) error {
	if len(man.Partitions) == 0 {
		return corrupt("manifest contains no partitions")
	}
	nodes := map[int]partitionNodeRef{}
	partitions := map[string]bool{}
	for _, p := range man.Partitions {
		if p.Name == "" {
			return corrupt("manifest contains empty partition name")
		}
		if partitions[p.Name] {
			return corrupt("manifest lists partition %q twice", p.Name)
		}
		partitions[p.Name] = true
	}
	for _, n := range man.Nodes {
		if n.ID < 1 {
			return corrupt("manifest contains invalid node id %d", n.ID)
		}
		if _, ok := nodes[n.ID]; ok {
			return corrupt("manifest lists node %d twice", n.ID)
		}
		if !partitions[n.Partition] {
			return corrupt("node %d belongs to missing partition %q", n.ID, n.Partition)
		}
		nodes[n.ID] = n
		if _, err := d.placeholder(n.Type, n.ID); err != nil {
			return err
		}
	}
	root, ok := nodes[man.Root.Ref]
	if !ok {
		return corrupt("manifest root $ref:%d is missing", man.Root.Ref)
	}
	_ = root
	for _, p := range man.Partitions {
		if len(p.Nodes) == 0 {
			return corrupt("partition %q has no nodes", p.Name)
		}
		seen := map[int]bool{}
		for _, id := range p.Nodes {
			n, ok := nodes[id]
			if !ok {
				return corrupt("partition %q references missing node %d", p.Name, id)
			}
			if n.Partition != p.Name {
				return corrupt("node %d partition mismatch", id)
			}
			if seen[id] {
				return corrupt("partition %q lists node %d twice", p.Name, id)
			}
			seen[id] = true
		}
	}
	return nil
}

func validatePartitionReferences(nodes []map[string]interface{}, known map[int]bool) error {
	for _, node := range nodes {
		data, ok := node[keyData].(map[string]interface{})
		if !ok {
			return fmt.Errorf("node missing data object")
		}
		if err := validateValueReferences(data, known); err != nil {
			return err
		}
	}
	return nil
}

func validateValueReferences(v interface{}, known map[int]bool) error {
	switch x := v.(type) {
	case map[string]interface{}:
		if ref, ok := refID(x); ok {
			if !known[ref] {
				return fmt.Errorf("dangling cross-partition reference $ref:%d", ref)
			}
			return nil
		}
		for _, child := range x {
			if err := validateValueReferences(child, known); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range x {
			if err := validateValueReferences(child, known); err != nil {
				return err
			}
		}
	}
	return nil
}

func refID(v map[string]interface{}) (int, bool) {
	r, ok := v[keyRef].(float64)
	if !ok {
		return 0, false
	}
	return int(r), true
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
