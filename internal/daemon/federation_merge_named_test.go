package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// TestFederator_MergeOriginsMap asserts the federated merge attaches a
// sibling origins map that tags which remote each merged node came from
// (and tags the local nodes "local").
func TestFederator_MergeOriginsMap(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[{"id":"r/x.go::RemoteOnly"},{"id":"shared::Sym"}],"edges":[],"total_nodes":2,"total_edges":0}`})
	local := envelope(`{"nodes":[{"id":"shared::Sym"},{"id":"l/a.go::LocalOnly"}],"edges":[],"total_nodes":2,"total_edges":0}`)

	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`),
		local, []ServerEntry{{Slug: "r2", URL: remote.URL}})

	m := decodeFederated(t, out)
	origins, ok := m["origins"].(map[string]any)
	if !ok {
		t.Fatalf("a federated merge must attach a sibling origins map, got %v", m["origins"])
	}
	// Every merged node id must be tagged with its provenance.
	want := map[string]string{
		"l/a.go::LocalOnly":  "local",
		"shared::Sym":        "local", // local wins on collision
		"r/x.go::RemoteOnly": "remote:r2",
	}
	for id, src := range want {
		if origins[id] != src {
			t.Errorf("origins[%q] = %v, want %q", id, origins[id], src)
		}
	}
	if len(origins) != len(want) {
		t.Errorf("origins map should tag exactly the %d merged nodes, got %d (%v)", len(want), len(origins), origins)
	}
}

// TestFederator_DeepCopiesNodes asserts the merge works on detached
// copies of remote node data: mutating a node in one merged response must
// never bleed into a subsequent merge of the same remote, proving no
// stored *graph.Node pointer is aliased across responses.
func TestFederator_DeepCopiesNodes(t *testing.T) {
	const remoteBody = `{"nodes":[{"id":"r/x.go::Caller","name":"OriginalName"}],"edges":[],"total_nodes":1,"total_edges":0}`
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteBody})
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	fed := testFederator()
	roster := []ServerEntry{{Slug: "r2", URL: remote.URL}}

	// First fan-out: pull the remote node into a merged SubGraph and mutate
	// it in place.
	out1 := fed.Augment(context.Background(), "find_usages", []byte(`{}`), local, roster)
	tool1, _ := unwrapToolJSON(out1)
	var sg1 query.SubGraph
	if err := json.Unmarshal(tool1, &sg1); err != nil {
		t.Fatalf("decode first merged subgraph: %v", err)
	}
	caller1 := findNamedNode(&sg1, "r/x.go::Caller")
	if caller1 == nil {
		t.Fatal("first merge must carry the remote node")
	}
	if caller1.Name != "OriginalName" {
		t.Fatalf("remote node name should round-trip, got %q", caller1.Name)
	}
	// Mutate the per-response node. If the federator aliased a shared,
	// stored pointer this would corrupt the source of every later merge.
	caller1.Name = "MUTATED"

	// Second fan-out of the same remote: the node must still carry the
	// original name, untouched by the first response's mutation.
	out2 := fed.Augment(context.Background(), "find_usages", []byte(`{}`), local, roster)
	tool2, _ := unwrapToolJSON(out2)
	var sg2 query.SubGraph
	if err := json.Unmarshal(tool2, &sg2); err != nil {
		t.Fatalf("decode second merged subgraph: %v", err)
	}
	caller2 := findNamedNode(&sg2, "r/x.go::Caller")
	if caller2 == nil {
		t.Fatal("second merge must carry the remote node")
	}
	if caller2.Name != "OriginalName" {
		t.Errorf("mutating a merged node must not bleed into a later response (deep copy): got %q, want %q",
			caller2.Name, "OriginalName")
	}
	// And the two responses must not share the same node pointer.
	if caller1 == caller2 {
		t.Error("each federated response must hand back its own node, not a shared pointer")
	}
}

func findNamedNode(sg *query.SubGraph, id string) *graph.Node {
	for _, n := range sg.Nodes {
		if n != nil && n.ID == id {
			return n
		}
	}
	return nil
}
