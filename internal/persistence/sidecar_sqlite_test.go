package persistence

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTempSidecar(t *testing.T) *SidecarStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	st, err := OpenSidecar(path)
	require.NoError(t, err)
	require.NotNil(t, st)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSidecar_OpenEmptyPathIsNoOp(t *testing.T) {
	st, err := OpenSidecar("")
	require.NoError(t, err)
	require.Nil(t, st)
}

func TestSidecar_SameAbsPathReusesHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	a, err := OpenSidecar(path)
	require.NoError(t, err)
	b, err := OpenSidecar(path)
	require.NoError(t, err)
	require.Same(t, a, b, "same absolute path must return the cached handle")
	t.Cleanup(func() { _ = a.Close() })
}

func TestSidecar_NotesRoundTrip(t *testing.T) {
	st := openTempSidecar(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	in := NoteEntry{
		ID:          "nt-1",
		Timestamp:   now,
		UpdatedAt:   now,
		SessionID:   "sess-1",
		ClientName:  "claude-code",
		Body:        "decision: switch to fastpath",
		SymbolID:    "pkg/foo.go::Bar",
		FilePath:    "pkg/foo.go",
		RepoPrefix:  "core",
		WorkspaceID: "ws-a",
		ProjectID:   "proj-a",
		Tags:        []string{"decision", "perf"},
		AutoLinks:   []string{"pkg/foo.go::Bar"},
		Pinned:      true,
	}
	require.NoError(t, st.UpsertNote("rk", in))

	rows, err := st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, in.ID, got.ID)
	assert.Equal(t, in.SessionID, got.SessionID)
	assert.Equal(t, in.ClientName, got.ClientName)
	assert.Equal(t, in.Body, got.Body)
	assert.Equal(t, in.SymbolID, got.SymbolID)
	assert.Equal(t, in.FilePath, got.FilePath)
	assert.Equal(t, in.WorkspaceID, got.WorkspaceID)
	assert.Equal(t, in.Tags, got.Tags)
	assert.Equal(t, in.AutoLinks, got.AutoLinks)
	assert.True(t, got.Pinned)
	assert.WithinDuration(t, in.UpdatedAt, got.UpdatedAt, time.Microsecond)

	// Scope isolation: another repo_key sees nothing.
	other, err := st.LoadNotesRows("other")
	require.NoError(t, err)
	require.Empty(t, other)

	// Delete.
	require.NoError(t, st.DeleteNote("rk", "nt-1"))
	rows, err = st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestSidecar_NotesTrimKeepsPinnedAndNewest(t *testing.T) {
	st := openTempSidecar(t)
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		require.NoError(t, st.UpsertNote("rk", NoteEntry{
			ID:        noteID(i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
			Pinned:    i == 0 || i == 5,
		}))
	}
	require.NoError(t, st.TrimNotes("rk", 6))

	rows, err := st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 6)
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[noteID(0)], "pinned[0] survives")
	assert.True(t, ids[noteID(5)], "pinned[5] survives")
	assert.True(t, ids[noteID(9)], "newest survives")
	assert.False(t, ids[noteID(1)], "oldest non-pinned dropped")
}

func TestSidecar_MemoriesRoundTrip(t *testing.T) {
	st := openTempSidecar(t)
	now := time.Now().UTC()
	in := MemoryEntry{
		ID:           "mem-1",
		Timestamp:    now,
		UpdatedAt:    now,
		LastAccessed: now,
		AccessCount:  7,
		Body:         "lock invariant for Bar",
		Title:        "Bar lock invariant",
		Kind:         "invariant",
		Source:       "manual",
		Confidence:   0.8,
		Importance:   5,
		AuthorAgent:  "claude-code",
		SymbolIDs:    []string{"pkg/foo.go::Bar"},
		FilePaths:    []string{"pkg/foo.go"},
		AutoLinks:    []string{"pkg/foo.go::Baz"},
		Tags:         []string{"invariant", "lock"},
		WorkspaceID:  "ws-a",
		ProjectID:    "proj-a",
		RepoPrefix:   "core",
		Pinned:       true,
		SupersededBy: "mem-2",
	}
	require.NoError(t, st.UpsertMemory("rk", in))

	rows, err := st.LoadMemoriesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, in.ID, got.ID)
	assert.Equal(t, in.Title, got.Title)
	assert.Equal(t, in.Kind, got.Kind)
	assert.Equal(t, in.Source, got.Source)
	assert.InDelta(t, in.Confidence, got.Confidence, 1e-6)
	assert.Equal(t, in.Importance, got.Importance)
	assert.Equal(t, in.AuthorAgent, got.AuthorAgent)
	assert.Equal(t, in.SymbolIDs, got.SymbolIDs)
	assert.Equal(t, in.FilePaths, got.FilePaths)
	assert.Equal(t, in.AutoLinks, got.AutoLinks)
	assert.Equal(t, in.Tags, got.Tags)
	assert.Equal(t, uint64(7), got.AccessCount)
	assert.Equal(t, "mem-2", got.SupersededBy)
	assert.True(t, got.Pinned)

	require.NoError(t, st.DeleteMemory("rk", "mem-1"))
	rows, err = st.LoadMemoriesRows("rk")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestSidecar_MemoriesTrimTwoPass(t *testing.T) {
	st := openTempSidecar(t)
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		e := MemoryEntry{
			ID:         memID(i),
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			UpdatedAt:  base.Add(time.Duration(i) * time.Second),
			Importance: 4,
		}
		if i == 2 || i == 4 {
			e.Importance = 1
		}
		if i == 7 {
			e.Pinned = true
			e.Importance = 1
		}
		require.NoError(t, st.UpsertMemory("rk", e))
	}
	require.NoError(t, st.TrimMemories("rk", 6))

	rows, err := st.LoadMemoriesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 6)
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[memID(7)], "pinned low-imp survives")
	assert.False(t, ids[memID(2)], "low-imp dropped")
	assert.False(t, ids[memID(4)], "low-imp dropped")
}

func TestSidecar_ScopesRoundTrip(t *testing.T) {
	st := openTempSidecar(t)
	require.NoError(t, st.UpsertScope(ScopeRow{
		Name: "backend", Description: "be", Repos: []string{"api", "core"}, Paths: []string{"services/x"},
	}))
	require.NoError(t, st.UpsertScope(ScopeRow{Name: "frontend", Repos: []string{"web"}}))

	rows, err := st.LoadScopes()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "backend", rows[0].Name)
	assert.Equal(t, []string{"api", "core"}, rows[0].Repos)
	assert.Equal(t, []string{"services/x"}, rows[0].Paths)
	assert.Equal(t, 2, st.ScopeCount())

	require.NoError(t, st.DeleteScope("backend"))
	rows, err = st.LoadScopes()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "frontend", rows[0].Name)
}

func TestSidecar_NotebookRoundTrip(t *testing.T) {
	st := openTempSidecar(t)
	now := time.Now().UTC()
	in := NotebookRow{
		ID:        "nb-1",
		Title:     "design: sidecar",
		Body:      "use sqlite\nfor durability",
		Tags:      []string{"design", "storage"},
		SymbolIDs: []string{"pkg/p.go::Q"},
		UsedCount: 3,
		LastUsed:  now,
		Created:   now,
		Updated:   now,
	}
	require.NoError(t, st.UpsertNotebook("rk", in))

	got, ok := st.GetNotebookRow("rk", "nb-1")
	require.True(t, ok)
	assert.Equal(t, in.Title, got.Title)
	assert.Equal(t, in.Body, got.Body)
	assert.Equal(t, in.Tags, got.Tags)
	assert.Equal(t, in.SymbolIDs, got.SymbolIDs)
	assert.Equal(t, uint64(3), got.UsedCount)

	rows, err := st.LoadNotebookRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.NoError(t, st.DeleteNotebook("rk", "nb-1"))
	_, ok = st.GetNotebookRow("rk", "nb-1")
	require.False(t, ok)
}

func TestSidecar_NotebookPrune(t *testing.T) {
	st := openTempSidecar(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	fresh := time.Now().UTC()
	require.NoError(t, st.UpsertNotebook("rk", NotebookRow{ID: "stale", Updated: old}))
	require.NoError(t, st.UpsertNotebook("rk", NotebookRow{ID: "fresh", Updated: fresh, LastUsed: fresh}))

	require.NoError(t, st.NotebookPrune("rk", time.Now().UTC().Add(-time.Hour)))
	rows, err := st.LoadNotebookRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "fresh", rows[0].ID)
}

// ---------------------------------------------------------------------------
// Migration: legacy gob.gz / json → sqlite.
// ---------------------------------------------------------------------------

func TestSidecar_MigrateLegacyNotes(t *testing.T) {
	legacyDir := t.TempDir()
	require.NoError(t, SaveNotes(legacyDir, &NoteStore{Entries: []NoteEntry{
		{ID: "nt-old", Body: "legacy note", SessionID: "s1", Pinned: true},
	}}))

	st := openTempSidecar(t)
	require.NoError(t, st.MigrateLegacyNotes("rk", legacyDir))

	rows, err := st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "nt-old", rows[0].ID)
	assert.Equal(t, "legacy note", rows[0].Body)
	assert.True(t, rows[0].Pinned)

	// Legacy file renamed to .bak.
	_, errOrig := os.Stat(filepath.Join(legacyDir, notesFile))
	assert.Error(t, errOrig, "original gob.gz must be renamed away")
	_, errBak := os.Stat(filepath.Join(legacyDir, notesFile+".bak"))
	assert.NoError(t, errBak, ".bak must exist")

	// Idempotent: a second migrate is a no-op (no duplicate rows).
	require.NoError(t, st.MigrateLegacyNotes("rk", legacyDir))
	rows, err = st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestSidecar_MigrateLegacyMemories(t *testing.T) {
	legacyDir := t.TempDir()
	require.NoError(t, SaveMemories(legacyDir, &MemoryStore{Entries: []MemoryEntry{
		{ID: "mem-old", Body: "legacy memory", Kind: "invariant", Importance: 5},
	}}))

	st := openTempSidecar(t)
	require.NoError(t, st.MigrateLegacyMemories("rk", legacyDir))

	rows, err := st.LoadMemoriesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "mem-old", rows[0].ID)
	assert.Equal(t, "invariant", rows[0].Kind)

	_, errBak := os.Stat(filepath.Join(legacyDir, memoriesFile+".bak"))
	assert.NoError(t, errBak, ".bak must exist")
}

func TestSidecar_MigrateLegacyScopes(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "scopes.json")
	require.NoError(t, os.WriteFile(legacyPath, []byte(`[{"name":"be","description":"backend","repos":["api"],"paths":["svc/x"]}]`), 0o644))

	st := openTempSidecar(t)
	require.NoError(t, st.MigrateLegacyScopes(legacyPath))

	rows, err := st.LoadScopes()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "be", rows[0].Name)
	assert.Equal(t, []string{"api"}, rows[0].Repos)
	assert.Equal(t, []string{"svc/x"}, rows[0].Paths)

	_, errBak := os.Stat(legacyPath + ".bak")
	assert.NoError(t, errBak)

	// Idempotent.
	require.NoError(t, st.MigrateLegacyScopes(legacyPath))
	assert.Equal(t, 1, st.ScopeCount())
}

func TestSidecar_MigrateLegacyNotebook(t *testing.T) {
	legacyDir := t.TempDir()
	md := "---\ntitle: old entry\ntags: [a, b]\nused_count: 4\n---\n\nbody text\n"
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "nbold.md"), []byte(md), 0o644))

	st := openTempSidecar(t)
	importMD := func(id, contents string) (NotebookRow, bool) {
		// Minimal frontmatter parse for the test importer.
		return NotebookRow{ID: id, Title: "old entry", Body: "body text\n", Tags: []string{"a", "b"}, UsedCount: 4}, true
	}
	require.NoError(t, st.MigrateLegacyNotebook("rk", legacyDir, importMD))

	rows, err := st.LoadNotebookRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "nbold", rows[0].ID)
	assert.Equal(t, "old entry", rows[0].Title)
	assert.Equal(t, uint64(4), rows[0].UsedCount)

	_, errBak := os.Stat(filepath.Join(legacyDir, "nbold.md.bak"))
	assert.NoError(t, errBak)
}

func TestSidecar_MigrateSkippedWhenTableNonEmpty(t *testing.T) {
	legacyDir := t.TempDir()
	require.NoError(t, SaveNotes(legacyDir, &NoteStore{Entries: []NoteEntry{{ID: "nt-old", Body: "legacy"}}}))

	st := openTempSidecar(t)
	// Pre-seed the table so the import is skipped (guard on existing rows).
	require.NoError(t, st.UpsertNote("rk", NoteEntry{ID: "nt-existing", Body: "already here"}))
	require.NoError(t, st.MigrateLegacyNotes("rk", legacyDir))

	rows, err := st.LoadNotesRows("rk")
	require.NoError(t, err)
	require.Len(t, rows, 1, "import must be skipped when the table already has rows")
	assert.Equal(t, "nt-existing", rows[0].ID)
}
