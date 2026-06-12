package tokens

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The age sweep removes stale entries from a shard and keeps fresh
// ones; read hits refresh an entry's mtime so live content survives.
func TestSweepShard_RemovesStaleKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	c := NewDiskCache(dir)

	shard := filepath.Join(dir, "ab")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(shard, "stale-entry")
	fresh := filepath.Join(shard, "fresh-entry")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("42"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * sweepMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	c.sweepShard(shard)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale entry should be swept, stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh entry must survive the sweep: %v", err)
	}
}

// A cache hit refreshes the entry's mtime, so the TTL behaves like LRU
// for content that is still flowing through the counters.
func TestCacheRead_RefreshesMtime(t *testing.T) {
	dir := t.TempDir()
	c := NewDiskCache(dir)

	content := make([]byte, minCacheBytes+1)
	for i := range content {
		content[i] = 'a'
	}
	s := string(content)
	want := c.Count(s) // miss → write

	key := c.key(s)
	path := c.pathFor(key)
	old := time.Now().Add(-2 * sweepMaxAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if got := c.Count(s); got != want {
		t.Fatalf("cached count = %d, want %d", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Errorf("read hit must refresh the entry mtime, got %v", info.ModTime())
	}
}
