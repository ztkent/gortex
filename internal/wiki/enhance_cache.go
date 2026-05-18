package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnhanceCache is a tiny disk-backed cache. Keys are derived from the
// stable inputs to one enhance call so re-running with the same
// graph + opts yields byte-identical output without invoking the
// provider. Misses fall through to the provider; hits short-circuit.
//
// Layout: <root>/<first-2-chars-of-key>/<key>.txt
// The file body is the enhanced markdown.
type EnhanceCache struct {
	root string
}

// NewEnhanceCache constructs a cache rooted at root. The directory is
// created lazily on first Set.
func NewEnhanceCache(root string) *EnhanceCache {
	return &EnhanceCache{root: root}
}

// DefaultEnhanceCacheDir returns the default cache location:
// ~/.cache/gortex/wiki-enhance (or $XDG_CACHE_HOME equivalent).
func DefaultEnhanceCacheDir() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); v != "" {
		return filepath.Join(v, "gortex", "wiki-enhance")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "gortex-wiki-enhance")
	}
	return filepath.Join(home, ".cache", "gortex", "wiki-enhance")
}

// Key derives a stable cache key for one section. It includes the
// prompt version so changing the prompt template busts the cache.
func (c *EnhanceCache) Key(section EnhanceSection, providerName string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v=%d\n", promptVersion)
	fmt.Fprintf(h, "provider=%s\n", providerName)
	fmt.Fprintf(h, "kind=%s\n", section.Kind)
	fmt.Fprintf(h, "title=%s\n", section.PageTitle)
	for _, a := range section.AnchorSymbolIDs {
		fmt.Fprintf(h, "anchor=%s\n", a)
	}
	fmt.Fprintf(h, "ctx=%s\n", section.Context)
	fmt.Fprintf(h, "body=%s\n", section.RawMarkdown)
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the cached enhanced markdown for the key.
// Returns ("", false, nil) on a clean miss, ("", false, err) on a
// real I/O error.
func (c *EnhanceCache) Get(key string) (string, bool, error) {
	if c == nil || c.root == "" {
		return "", false, nil
	}
	path := c.pathFor(key)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(body), true, nil
}

// Set writes the enhanced markdown for key.
func (c *EnhanceCache) Set(key, body string) error {
	if c == nil || c.root == "" {
		return nil
	}
	path := c.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func (c *EnhanceCache) pathFor(key string) string {
	if len(key) < 2 {
		return filepath.Join(c.root, key+".txt")
	}
	return filepath.Join(c.root, key[:2], key+".txt")
}
