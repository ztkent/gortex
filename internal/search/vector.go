package search

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/coder/hnsw"
)

// vectorFrameMagic prefixes the framed VectorBackend.Save format: a
// 4-byte magic, a uint32 chunk-map JSON length, the chunk-map JSON,
// then the raw HNSW export. A blob lacking the magic is a legacy
// (pre-chunking) raw HNSW export and is loaded with an empty chunk
// map — so old snapshots keep working.
var vectorFrameMagic = [4]byte{'G', 'V', 'X', '1'}

// VectorBackend stores and searches embedding vectors using HNSW index.
type VectorBackend struct {
	graph *hnsw.Graph[string]
	count int
	dims  int
	// chunkMap maps a synthetic chunk vector ID ("<symbolID>#chunkK")
	// to its parent symbol ID. It is non-empty only when AST
	// sub-chunking split one or more symbols into multiple vectors.
	// Search results are de-chunked through it so a symbol is never
	// returned twice and chunk IDs never leak to callers.
	chunkMap map[string]string
	mu       sync.RWMutex
}

// NewVector creates a vector search backend for the given embedding dimensions.
func NewVector(dims int) *VectorBackend {
	g := hnsw.NewGraph[string]()
	g.Distance = hnsw.CosineDistance
	return &VectorBackend{
		graph: g,
		dims:  dims,
	}
}

// SetChunkMap installs the chunk-vector → parent-symbol mapping. Called
// by the indexer after a chunked vector build. A nil or empty map
// means no symbol was sub-chunked.
func (v *VectorBackend) SetChunkMap(m map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.chunkMap = m
}

// ResolveChunk maps a vector ID to the symbol ID a caller should see.
// For a chunk vector it returns the parent symbol ID; for a plain
// symbol vector it returns the ID unchanged. The bool reports whether
// the ID was a chunk.
func (v *VectorBackend) ResolveChunk(id string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if parent, ok := v.chunkMap[id]; ok {
		return parent, true
	}
	return id, false
}

// HasChunks reports whether any vector in the index is a chunk.
func (v *VectorBackend) HasChunks() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.chunkMap) > 0
}

// Add indexes a symbol with its embedding vector.
func (v *VectorBackend) Add(id string, vector []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.graph.Add(hnsw.Node[string]{
		Key:   id,
		Value: hnsw.Vector(vector),
	})
	v.count++
}

// Search returns the k nearest neighbors to the query vector.
func (v *VectorBackend) Search(query []float32, k int) []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.count == 0 {
		return nil
	}
	results := v.graph.Search(hnsw.Vector(query), k)
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.Key
	}
	return ids
}

// Count returns the number of indexed vectors.
func (v *VectorBackend) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.count
}

// Dims returns the embedding dimensionality.
func (v *VectorBackend) Dims() int { return v.dims }

// SizeBytes estimates HNSW's heap footprint. The raw vector storage
// (dims × 4 B) is a small fraction of the total — each node also
// carries layer neighbor lists, priority-queue scratch, and the
// string-keyed maps that drive graph navigation. Calibrated against
// heap profiles on a 68k-vector index (50 dims, default M=16): live
// was ~408 MiB, i.e. ~6 KiB per vector overall. Using dims×4 + 5900 B
// keeps the formula honest as dims change (MiniLM at 384 would push
// the dims×4 term to 1.5 KiB per vector, overhead stays roughly flat).
func (v *VectorBackend) SizeBytes() uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.count == 0 {
		return 0
	}
	const hnswOverhead = 5900 // neighbor lists + map headers + priority-queue slack
	perVector := uint64(v.dims)*4 + hnswOverhead
	return uint64(v.count) * perVector
}

// Save writes the vector index to a writer in the framed format:
// magic + chunk-map JSON + raw HNSW export. The chunk map is persisted
// so query-time de-chunking still works after a daemon restart.
func (v *VectorBackend) Save(w io.Writer) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	mapJSON := []byte("{}")
	if len(v.chunkMap) > 0 {
		b, err := json.Marshal(v.chunkMap)
		if err != nil {
			return fmt.Errorf("marshal chunk map: %w", err)
		}
		mapJSON = b
	}
	if _, err := w.Write(vectorFrameMagic[:]); err != nil {
		return fmt.Errorf("write vector frame magic: %w", err)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(mapJSON)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write chunk map length: %w", err)
	}
	if _, err := w.Write(mapJSON); err != nil {
		return fmt.Errorf("write chunk map: %w", err)
	}
	if err := v.graph.Export(w); err != nil {
		return fmt.Errorf("export vector index: %w", err)
	}
	return nil
}

// LoadFrom restores the vector index from a reader. It accepts both
// the framed format (magic + chunk map + HNSW) and the legacy raw
// HNSW export written before AST sub-chunking shipped — a legacy blob
// has no magic and loads with an empty chunk map.
func (v *VectorBackend) LoadFrom(r io.Reader) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Buffer the whole blob so a missing magic can be replayed into the
	// HNSW importer. Vector index blobs are small relative to the graph.
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read vector index: %w", err)
	}
	hnswBytes := all
	v.chunkMap = nil
	if len(all) >= 8 && bytes.Equal(all[:4], vectorFrameMagic[:]) {
		mapLen := binary.LittleEndian.Uint32(all[4:8])
		if int(mapLen)+8 > len(all) {
			return fmt.Errorf("vector index frame: chunk map length %d exceeds blob", mapLen)
		}
		mapJSON := all[8 : 8+mapLen]
		hnswBytes = all[8+mapLen:]
		if mapLen > 0 {
			m := make(map[string]string)
			if err := json.Unmarshal(mapJSON, &m); err != nil {
				return fmt.Errorf("unmarshal chunk map: %w", err)
			}
			if len(m) > 0 {
				v.chunkMap = m
			}
		}
	}
	if err := v.graph.Import(bytes.NewReader(hnswBytes)); err != nil {
		return fmt.Errorf("import vector index: %w", err)
	}
	return nil
}

// SetCount sets the node count (used after loading from persistence).
func (v *VectorBackend) SetCount(n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.count = n
}
