package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestWireContractFingerprint is the schema-stability guard for the
// daemon's snapshot wire format. It fingerprints the exported fields of
// every struct that gets gob-encoded into the snapshot and compares the
// hash against a checked-in golden value. Any change to a field name,
// type, or set-membership shifts the hash.
//
// When this test fails:
//
//  1. Additive change (new field only, existing fields untouched) —
//     update the golden value below. Old snapshots still load because
//     gob decodes unknown fields as zero.
//
//  2. Breaking change (rename, remove, retype an existing field) —
//     bump snapshotSchemaVersion in daemon_snapshot.go AND register a
//     migration in snapshotMigrations, THEN update the golden value.
//     Without a migration, deployed daemons reading an old snapshot
//     will discard the cache on upgrade and pay the full-reindex cost.
//
// Runs as part of the existing `go test ./...` sweep, no extra CI
// infrastructure required.
func TestWireContractFingerprint(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"graph.Node", reflect.TypeOf(graph.Node{}), ""},
		{"graph.Edge", reflect.TypeOf(graph.Edge{}), ""},
		{"snapshotHeader", reflect.TypeOf(snapshotHeader{}), ""},
		{"snapshotRepo", reflect.TypeOf(snapshotRepo{}), ""},
		{"snapshotContract", reflect.TypeOf(snapshotContract{}), ""},
	}

	// Golden values computed by fingerprintType against the current
	// struct definitions. When a change to any of these structs is
	// intentional, update this map with the new values (run the test
	// once, copy the "got" hash from the failure message).
	golden := map[string]string{
		"graph.Node":       wireContractGolden("graph.Node"),
		"graph.Edge":       wireContractGolden("graph.Edge"),
		"snapshotHeader":   wireContractGolden("snapshotHeader"),
		"snapshotRepo":     wireContractGolden("snapshotRepo"),
		"snapshotContract": wireContractGolden("snapshotContract"),
	}

	for i := range cases {
		cases[i].want = golden[cases[i].name]
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fingerprintType(c.typ)
			if got != c.want {
				t.Errorf(`wire contract for %s changed.
  got:  %s
  want: %s

If the change is ADDITIVE (new field, existing fields untouched):
  • Old snapshots still decode cleanly (gob reads unknown fields as zero).
  • Update the golden fingerprint in wireContractGolden().

If the change RENAMES / REMOVES / RETYPES an existing field:
  • Old snapshots will fail to decode cleanly on the changed field.
  • Bump snapshotSchemaVersion in daemon_snapshot.go.
  • Register a migration in snapshotMigrations for the old→new version.
  • Then update the golden fingerprint.

Field set: %s`, c.name, got, c.want, describeFields(c.typ))
			}
		})
	}
}

// fingerprintType returns a stable SHA-256 over the exported-field set
// of t. Field order is NOT part of the fingerprint (gob identifies
// fields by name), but name + type are. Nested structs are captured by
// their type.String() — if a nested type's own shape changes, callers
// with their own fingerprint will catch it.
func fingerprintType(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return ""
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	h := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(h[:])
}

// describeFields renders the exported-field set for failure messages.
// Matches fingerprintType's canonical form so the output is a drop-in
// replacement for the hash during debugging.
func describeFields(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return "(not a struct)"
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	return strings.Join(fields, ", ")
}

// wireContractGolden holds the expected fingerprint for each wire
// type. Updated intentionally when a struct changes; see the doc on
// TestWireContractFingerprint for the decision tree (additive update vs
// schema bump + migration). Values are pinned hashes — NOT recomputed
// from the live type — so a field-level drift will surface here.
func wireContractGolden(name string) string {
	switch name {
	case "graph.Node":
		// Bumped when the federation proxy-node fields Origin / Stub /
		// FetchedAt were added — written only by the (gated, not-yet-built)
		// cross-daemon mint path; empty on every locally-indexed node.
		// Additive: gob decodes unknown fields as zero, so older snapshots
		// still load with them blank, and proxy nodes are never persisted.
		// Previously bumped when AbsoluteFilePath, then WorkspaceID /
		// ProjectID, were added.
		return "3b8920ab88d05028e215d68d5917445e2e6d05bdad23aef6dcdf6c9920647823"
	case "graph.Edge":
		// Bumped when Context was added — the per-reference role label
		// (parameter_type / return_type / field / …) populated on demand by
		// find_usages via RefContextOf. Additive: gob decodes older
		// snapshots with Context blank, and it is recomputed at query time.
		// (Previously bumped when Tier was added.)
		return "ed897cce4720cd1482d8c217ba5ffb72d7f19d5d1c2d4015b9a98e9daa9d4b63"
	case "snapshotHeader":
		// Bumped when the VectorIndex / VectorDims / VectorCount fields
		// were added (additive — gob decodes unknown fields as zero).
		// This change shipped alongside snapshotSchemaVersion v3 and a
		// v2→v3 migration so deployed daemons keep their v2 snapshots
		// across the upgrade instead of paying a full re-index.
		// Previously bumped when BinaryMtimeUnix was added.
		return "a0f604616d04c757ffec4d084fc53cf6c3db03c8add1a8af0f32f902c1b1fe92"
	case "snapshotRepo":
		return "8a78544c6e8d6c384f95b971df43408e7bc5f5ab4c7f2052d038bd3ffa4e1311"
	case "snapshotContract":
		// New wire type introduced with per-repo contract persistence.
		// Mirrors contracts.Contract but stores Type/Role as strings so
		// the snapshot stays decoupled from runtime type aliases.
		return "17b073a2f334b46d5f360a15f3d5d1617bee20660221f1de513735cedd75799c"
	default:
		panic(fmt.Sprintf("no golden fingerprint for %s", name))
	}
}
