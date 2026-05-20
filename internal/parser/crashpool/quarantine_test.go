package crashpool

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuarantine_LoadMissingFile(t *testing.T) {
	q := LoadQuarantine(filepath.Join(t.TempDir(), "nope.json"))
	require.Equal(t, 0, q.Len())
	require.False(t, q.IsQuarantined("a.go", 1))
}

func TestQuarantine_AddAndCheck(t *testing.T) {
	q := LoadQuarantine("")
	q.Add("bad.go", "SIGSEGV", 100)

	// Same revision → quarantined (skip on retry).
	require.True(t, q.IsQuarantined("bad.go", 100))
	// Changed revision → not quarantined (gets one retry).
	require.False(t, q.IsQuarantined("bad.go", 200))
	// Unknown file → never quarantined.
	require.False(t, q.IsQuarantined("good.go", 100))
}

func TestQuarantine_ReAddBumpsAttempts(t *testing.T) {
	q := LoadQuarantine("")
	q.Add("bad.go", "crash", 100)
	q.Add("bad.go", "crash again", 200)

	entries := q.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, 2, entries[0].Attempts)
	require.Equal(t, int64(200), entries[0].MtimeNano)
	require.Equal(t, "crash again", entries[0].Reason)
}

func TestQuarantine_Forget(t *testing.T) {
	q := LoadQuarantine("")
	q.Add("bad.go", "crash", 100)
	require.True(t, q.IsQuarantined("bad.go", 100))
	q.Forget("bad.go")
	require.False(t, q.IsQuarantined("bad.go", 100))
	require.Equal(t, 0, q.Len())
}

func TestQuarantine_SaveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "quarantine.json")
	q := LoadQuarantine(path)
	q.Add("a.go", "boom", 1)
	q.Add("b.go", "hang", 2)
	require.NoError(t, q.Save())

	reloaded := LoadQuarantine(path)
	require.Equal(t, 2, reloaded.Len())
	require.True(t, reloaded.IsQuarantined("a.go", 1))
	require.True(t, reloaded.IsQuarantined("b.go", 2))

	entries := reloaded.Entries()
	require.Equal(t, "a.go", entries[0].RelPath) // sorted
	require.Equal(t, "b.go", entries[1].RelPath)
}

func TestQuarantine_NilSafe(t *testing.T) {
	var q *Quarantine
	require.False(t, q.IsQuarantined("x", 1))
	require.Equal(t, 0, q.Len())
	require.Nil(t, q.Entries())
	q.Add("x", "y", 1) // no panic
	q.Forget("x")      // no panic
	require.NoError(t, q.Save())
}

func TestQuarantine_EmptyPathSaveNoop(t *testing.T) {
	q := LoadQuarantine("")
	q.Add("a.go", "x", 1)
	require.NoError(t, q.Save()) // no file, no error
}

func TestQuarantine_CorruptFileTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json at all"), 0o644))
	q := LoadQuarantine(path)
	require.Equal(t, 0, q.Len()) // corrupt → treated as empty
}

// TestQuarantine_ConcurrentSave runs many Saves at once: a shared
// quarantine is saved per indexed file across parallel re-indexes, so
// the unique temp-file name must keep concurrent writes from
// clobbering each other and leave a final file that decodes cleanly.
func TestQuarantine_ConcurrentSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")
	q := LoadQuarantine(path)
	q.Add("bad.go", "SIGSEGV", 123)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, q.Save())
		}()
	}
	wg.Wait()

	reloaded := LoadQuarantine(path)
	require.Equal(t, 1, reloaded.Len())
	require.True(t, reloaded.IsQuarantined("bad.go", 123))
}
