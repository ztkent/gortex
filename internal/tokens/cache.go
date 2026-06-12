// cache.go — a content-addressed disk cache for token counts.
//
// Count runs a full BPE tokenization on every call. For file- and
// symbol-sized content the daemon re-counts the same bytes repeatedly:
// identical source flows through get_symbol_source / read_file /
// export_context across a session, and again across daemon restarts.
// This cache keys a count by a SHA-256 of (tokenizer revision +
// content), so an unchanged blob costs one file read instead of a
// re-tokenization. A revision change makes every old key unreachable,
// so counts produced by a different tokenizer are never trusted.
package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// tokenCacheFormat is the on-disk cache format version. Bump it to
// invalidate every cached count after a format change.
const tokenCacheFormat = "1"

// minCacheBytes is the content size below which CachedCount skips the
// disk round-trip: for small inputs the SHA-256 + file read costs more
// than the tokenization it would save.
const minCacheBytes = 2048

// sweepEvery / sweepMaxAge bound the cache's growth: every sweepEvery-th
// write prunes the shard directory just written of entries older than
// sweepMaxAge. Entries are one content version each and never reused
// once the content changes, so without a sweep the cache grows one
// inode per unique payload forever. Read hits refresh the entry's
// mtime, so the TTL approximates LRU for content that is still live.
const (
	sweepEvery  = 64
	sweepMaxAge = 30 * 24 * time.Hour
)

// DiskCache is a content-addressed token-count cache backed by a
// directory tree. It is safe for concurrent use — entries are written
// atomically (temp + rename) and a torn or absent entry simply falls
// through to a fresh Count.
type DiskCache struct {
	dir      string
	revision string
	writes   atomic.Uint64
}

// DefaultTokenCacheDir returns the default cache location:
// ~/.gortex/cache/token-counts (or the $XDG_CACHE_HOME equivalent).
func DefaultTokenCacheDir() string {
	return filepath.Join(platform.CacheDir(), "token-counts")
}

// NewDiskCache builds a token-count cache rooted at dir. An empty dir
// uses DefaultTokenCacheDir. The cache revision is bound to the active
// tokenizer (cl100k_base, or the chars/4 fallback when the encoder is
// unavailable) so counts from the two modes never collide.
func NewDiskCache(dir string) *DiskCache {
	if dir == "" {
		dir = DefaultTokenCacheDir()
	}
	mode := "len4"
	if EncoderReady() {
		mode = "cl100k_base"
	}
	return &DiskCache{dir: dir, revision: tokenCacheFormat + "/" + mode}
}

// Count returns the token count of s, reading it from disk when a prior
// run already counted identical bytes and writing a fresh entry on a
// miss. A nil cache, or any disk error, falls through to a direct
// Count — caching never changes the returned value, only its cost.
func (c *DiskCache) Count(s string) int {
	if c == nil {
		return Count(s)
	}
	key := c.key(s)
	if n, ok := c.read(key); ok {
		return n
	}
	n := Count(s)
	c.write(key, n)
	return n
}

// key is the SHA-256 of the tokenizer revision and the content. The
// revision is folded into the hash, so a revision change yields wholly
// different keys: old-revision entries become unreachable rather than
// being mistaken for current counts.
func (c *DiskCache) key(s string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(c.revision))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// pathFor shards entries one level deep by the key's first two hex
// digits so a single directory never holds the whole cache.
func (c *DiskCache) pathFor(key string) string {
	return filepath.Join(c.dir, key[:2], key)
}

func (c *DiskCache) read(key string) (int, bool) {
	path := c.pathFor(key)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0, false
	}
	// Refresh the entry so the age sweep approximates LRU: content that
	// is still being counted stays; content that stopped flowing ages out.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return n, true
}

func (c *DiskCache) write(key string, n int) {
	path := c.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tc-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(n)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if c.writes.Add(1)%sweepEvery == 0 {
		c.sweepShard(filepath.Dir(path))
	}
}

// sweepShard removes entries in one shard directory whose mtime is
// older than sweepMaxAge. Best-effort and concurrent-safe by
// construction: a deleted entry is just a future cache miss.
func (c *DiskCache) sweepShard(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sweepMaxAge)
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || info.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

var (
	defaultCache     *DiskCache
	defaultCacheOnce sync.Once
)

func defaultDiskCache() *DiskCache {
	defaultCacheOnce.Do(func() { defaultCache = NewDiskCache("") })
	return defaultCache
}

// CachedCount counts the tokens in s, consulting a process-wide
// content-addressed disk cache for inputs large enough that the disk
// round-trip beats re-tokenizing. Small inputs are counted directly.
// The result is identical to Count — only repeated counts of the same
// bytes get cheaper.
func CachedCount(s string) int {
	if len(s) < minCacheBytes {
		return Count(s)
	}
	return defaultDiskCache().Count(s)
}

// CachedCountInt64 is CachedCount for call sites that store counts as
// int64 (e.g. cumulative session metrics).
func CachedCountInt64(s string) int64 {
	return int64(CachedCount(s))
}
