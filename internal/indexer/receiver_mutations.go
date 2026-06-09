package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// indirectMutSpec is one synthesized indirect field-mutation edge: `from`
// (a method) mutates the field `to` indirectly by calling `via` on it (or on
// its own receiver, which transitively mutates the field).
type indirectMutSpec struct {
	from, to, file, via string
	line                int
}

// stdlibMutators are method names whose bodies are invisible to a source-level
// pass (they live in compiled stdlib export data) but are known to mutate their
// receiver: sync/atomic, sync.Mutex/RWMutex, WaitGroup, Once, sync.Map. A call
// to one of these on a receiver field is an indirect mutation of that field.
var stdlibMutators = map[string]bool{
	// sync/atomic.*
	"Store": true, "Swap": true, "CompareAndSwap": true, "Add": true,
	// sync.Mutex / RWMutex
	"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true, "TryLock": true,
	// sync.WaitGroup
	"Done": true,
	// sync.Once
	"Do": true,
	// sync.Map
	"Delete": true, "LoadOrStore": true, "LoadAndDelete": true,
}

func isStdlibMutator(name string) bool { return stdlibMutators[name] }

// bareCallName returns the trailing method name of an edge target id, stripping
// any repo / unresolved / package qualifier ("unresolved::*.Store" → "Store").
func bareCallName(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	if i := strings.LastIndex(id, "."); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// indirectMutationEdges computes, over the resolved graph, the indirect field
// mutations: `s.counter.Increment()` mutates field counter because Increment
// mutates its receiver; `s.helper()` mutates s's fields because helper does.
// A transitive fixpoint propagates "this method mutates its receiver" through
// own-receiver field-method calls and sibling-method calls — the piece gograph
// explicitly defers. Pure graph traversal, no SSA, no type-checking.
func indirectMutationEdges(g graph.Store) []indirectMutSpec {
	if g == nil {
		return nil
	}
	recvType := map[string]string{} // methodID → its receiver type
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Meta == nil {
			continue
		}
		if rt, _ := n.Meta["receiver"].(string); rt != "" {
			recvType[n.ID] = rt
		}
	}
	if len(recvType) == 0 {
		return nil
	}
	// fieldByOwner[receiverType][fieldName] = fieldID.
	fieldByOwner := map[string]map[string]string{}
	for n := range g.NodesByKind(graph.KindField) {
		if n == nil || n.Meta == nil {
			continue
		}
		owner, _ := n.Meta["receiver"].(string)
		if owner == "" || n.Name == "" {
			continue
		}
		if fieldByOwner[owner] == nil {
			fieldByOwner[owner] = map[string]string{}
		}
		fieldByOwner[owner][n.Name] = n.ID
	}

	// mutators[methodID] = set of own-receiver field names it mutates.
	mutators := map[string]map[string]bool{}
	addMut := func(m, f string) bool {
		if mutators[m] == nil {
			mutators[m] = map[string]bool{}
		}
		if mutators[m][f] {
			return false
		}
		mutators[m][f] = true
		return true
	}
	// Seed: a method that directly writes a field of its own receiver type.
	for e := range g.EdgesByKind(graph.EdgeWrites) {
		if e == nil {
			continue
		}
		owner, ok := recvType[e.From]
		if !ok {
			continue
		}
		fn := g.GetNode(e.To)
		if fn == nil || fn.Kind != graph.KindField {
			continue
		}
		if fowner, _ := fn.Meta["receiver"].(string); fowner == owner {
			addMut(e.From, fn.Name)
		}
	}

	// Collect own-receiver calls once (recv_field or recv_self stamped).
	type ocall struct {
		from, calleeID, calleeName, recvField string
		recvSelf                              bool
		file                                  string
		line                                  int
	}
	var ocalls []ocall
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		rf, _ := e.Meta["recv_field"].(string)
		rs, _ := e.Meta["recv_self"].(bool)
		if rf == "" && !rs {
			continue
		}
		if _, ok := recvType[e.From]; !ok {
			continue
		}
		name := bareCallName(e.To)
		if cn := g.GetNode(e.To); cn != nil && cn.Kind == graph.KindMethod && cn.Name != "" {
			name = cn.Name
		}
		ocalls = append(ocalls, ocall{
			from: e.From, calleeID: e.To, calleeName: name,
			recvField: rf, recvSelf: rs, file: e.FilePath, line: e.Line,
		})
	}

	// Transitive fixpoint.
	for {
		changed := false
		for _, c := range ocalls {
			calleeMutates := len(mutators[c.calleeID]) > 0
			switch {
			case c.recvField != "":
				if !calleeMutates && !isStdlibMutator(c.calleeName) {
					continue
				}
				owner := recvType[c.from]
				if fieldByOwner[owner][c.recvField] == "" {
					continue // type-consistency: caller's receiver has this field
				}
				if addMut(c.from, c.recvField) {
					changed = true
				}
			case c.recvSelf:
				// Sibling method call on the own receiver: callee must be a
				// method of the same receiver type, and it must mutate.
				if !calleeMutates || recvType[c.calleeID] != recvType[c.from] || recvType[c.from] == "" {
					continue
				}
				for f := range mutators[c.calleeID] {
					if addMut(c.from, f) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	// Emit indirect accesses_field edges.
	var out []indirectMutSpec
	seen := map[string]bool{}
	emit := func(from, fieldID, file, via string, line int) {
		k := from + "\x00" + fieldID + "\x00" + via
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, indirectMutSpec{from: from, to: fieldID, file: file, via: via, line: line})
	}
	for _, c := range ocalls {
		owner := recvType[c.from]
		calleeMutates := len(mutators[c.calleeID]) > 0
		switch {
		case c.recvField != "":
			if !calleeMutates && !isStdlibMutator(c.calleeName) {
				continue
			}
			if fid := fieldByOwner[owner][c.recvField]; fid != "" {
				emit(c.from, fid, c.file, c.calleeName, c.line)
			}
		case c.recvSelf && calleeMutates:
			if recvType[c.calleeID] != owner || owner == "" {
				continue
			}
			for f := range mutators[c.calleeID] {
				if fid := fieldByOwner[owner][f]; fid != "" {
					emit(c.from, fid, c.file, c.calleeName, c.line)
				}
			}
		}
	}
	return out
}
