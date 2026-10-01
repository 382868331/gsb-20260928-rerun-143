package savecore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"reflect"
	"sort"
	"strings"
)

const partitionFormatVersion = 2

// Partitioner chooses a partition name for a registered object type.
type Partitioner func(typeName string) string

type partitionManifest struct {
	Format     string             `json:"format"`
	FormatVer  int                `json:"formatVer"`
	Root       partitionRef       `json:"root"`
	Partitions []partitionInfo    `json:"partitions"`
	Nodes      []partitionNodeRef `json:"nodes"`
}

type partitionRef struct {
	Ref int `json:"$ref"`
}

type partitionInfo struct {
	Name  string `json:"name"`
	Nodes []int  `json:"nodes"`
	CRC   uint32 `json:"crc32"`
}

type partitionNodeRef struct {
	ID        int    `json:"$id"`
	Type      string `json:"$type"`
	Ver       int    `json:"$ver"`
	Partition string `json:"partition"`
}

type partitionFile struct {
	Format    string                   `json:"format"`
	FormatVer int                      `json:"formatVer"`
	Partition string                   `json:"partition"`
	Nodes     []map[string]interface{} `json:"nodes"`
}

type graphData struct {
	RootID     int
	Nodes      map[int]map[string]interface{}
	Metadata   map[int]partitionNodeRef
	Partitions map[string][]int
}

// PartitionCorruptionError identifies a physical partition and the object
// identities that cannot be recovered from it.
type PartitionCorruptionError struct {
	Partition   string
	ObjectIDs   []int
	ObjectTypes []string
	Reason      string
}

func (e *PartitionCorruptionError) Error() string {
	return fmt.Sprintf("savecore: partition %q is unrecoverable (%s); objects ids=%v types=%s",
		e.Partition, e.Reason, e.ObjectIDs, strings.Join(e.ObjectTypes, ","))
}

// NewPartitionedStore stores a snapshot as a checksummed manifest plus
// partition files. Complete generations are published atomically.
func NewPartitionedStore(reg *Registry, dir string, partitioner Partitioner) *Store {
	if partitioner == nil {
		panic("savecore: partitioned store requires a partitioner")
	}
	return &Store{reg: reg, Path: dir, partitionFor: partitioner}
}

func (r *Registry) encodePartitioned(root any, partitioner Partitioner) (*graphData, []byte, []partitionFile, error) {
	if root == nil {
		return nil, nil, nil, fmt.Errorf("savecore: cannot encode nil root")
	}
	rv := reflect.ValueOf(root)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return nil, nil, nil, fmt.Errorf("savecore: root must be a non-nil pointer")
	}
	e := &partitionEncoder{
		reg:         r,
		types:       map[reflect.Type]string{},
		seen:        map[ptrKey]int{},
		nodes:       map[int]map[string]interface{}{},
		metadata:    map[int]partitionNodeRef{},
		partitions:  map[string][]int{},
		partitioner: partitioner,
	}
	for name, c := range r.codecs {
		e.types[reflect.TypeOf(c.New()).Elem()] = name
	}
	rootID, err := e.encodePtr(rv)
	if err != nil {
		return nil, nil, nil, err
	}
	names := make([]string, 0, len(e.partitions))
	for name := range e.partitions {
		names = append(names, name)
	}
	sort.Strings(names)
	infos := make([]partitionInfo, 0, len(names))
	files := make([]partitionFile, 0, len(names))
	refs := make([]partitionNodeRef, 0, len(e.metadata))
	ids := make([]int, 0, len(e.metadata))
	for id := range e.metadata {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		refs = append(refs, e.metadata[id])
	}
	for _, name := range names {
		ids := e.partitions[name]
		sort.Ints(ids)
		nodes := make([]map[string]interface{}, 0, len(ids))
		for _, id := range ids {
			nodes = append(nodes, e.nodes[id])
		}
		pf := partitionFile{Format: formatName, FormatVer: partitionFormatVersion, Partition: name, Nodes: nodes}
		raw, err := json.Marshal(pf)
		if err != nil {
			return nil, nil, nil, err
		}
		files = append(files, pf)
		infos = append(infos, partitionInfo{Name: name, Nodes: append([]int(nil), ids...), CRC: crc32Bytes(raw)})
	}
	man := partitionManifest{
		Format: formatName, FormatVer: partitionFormatVersion,
		Root: partitionRef{Ref: rootID}, Partitions: infos, Nodes: refs,
	}
	manifestRaw, err := json.Marshal(man)
	if err != nil {
		return nil, nil, nil, err
	}
	return &graphData{RootID: rootID, Nodes: e.nodes, Metadata: e.metadata, Partitions: e.partitions}, manifestRaw, files, nil
}

type partitionEncoder struct {
	reg         *Registry
	types       map[reflect.Type]string
	seen        map[ptrKey]int
	nodes       map[int]map[string]interface{}
	metadata    map[int]partitionNodeRef
	partitions  map[string][]int
	partitioner Partitioner
	next        int
}

func (e *partitionEncoder) encodePtr(rv reflect.Value) (int, error) {
	key := ptrKey{ptr: rv.Pointer(), typ: rv.Elem().Type()}
	if id, ok := e.seen[key]; ok {
		return id, nil
	}
	name, ok := e.types[rv.Elem().Type()]
	if !ok {
		return 0, fmt.Errorf("savecore: type %s is not registered", rv.Elem().Type())
	}
	partition := e.partitioner(name)
	if partition == "" || strings.ContainsAny(partition, `/\`) {
		return 0, fmt.Errorf("savecore: invalid partition name %q", partition)
	}
	e.next++
	id := e.next
	e.seen[key] = id
	data, err := e.encodeStruct(rv.Elem())
	if err != nil {
		return 0, err
	}
	e.nodes[id] = map[string]interface{}{keyID: id, keyType: name, keyVer: e.reg.codecs[name].Version, keyData: data}
	e.metadata[id] = partitionNodeRef{ID: id, Type: name, Ver: e.reg.codecs[name].Version, Partition: partition}
	e.partitions[partition] = append(e.partitions[partition], id)
	return id, nil
}

func (e *partitionEncoder) refValue(rv reflect.Value) (map[string]interface{}, error) {
	id, err := e.encodePtr(rv)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{keyRef: id}, nil
}

func (e *partitionEncoder) encodeStruct(rv reflect.Value) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		v, err := e.encodeValue(rv.Field(i))
		if err != nil {
			return nil, fmt.Errorf("field %s.%s: %w", rv.Type().Name(), f.Name, err)
		}
		out[f.Name] = v
	}
	return out, nil
}

func (e *partitionEncoder) encodeValue(rv reflect.Value) (interface{}, error) {
	switch rv.Kind() {
	case reflect.Ptr:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("pointer to %s is not a registered object", rv.Elem().Kind())
		}
		return e.refValue(rv)
	case reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return nil, fmt.Errorf("interface fields are not supported (prevents hidden code execution)")
	case reflect.Struct:
		return e.encodeStruct(rv)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, nil
		}
		out := make([]interface{}, rv.Len())
		for i := range out {
			v, err := e.encodeValue(rv.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case reflect.Map:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("only string-keyed maps are supported")
		}
		out := map[string]interface{}{}
		iter := rv.MapRange()
		for iter.Next() {
			v, err := e.encodeValue(iter.Value())
			if err != nil {
				return nil, err
			}
			out[iter.Key().String()] = v
		}
		return out, nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	default:
		return nil, fmt.Errorf("unsupported kind %s", rv.Kind())
	}
}

func crc32Bytes(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}

func newGenerationID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "gen-" + hex.EncodeToString(b[:])
}
