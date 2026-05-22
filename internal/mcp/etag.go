package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
)

// computeETag produces a short content hash suitable for conditional fetch.
// The hash is computed from the JSON serialization of the data.
func computeETag(data any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8]) // 16 hex chars — collision-safe for session use
}

// notModifiedResult returns a minimal "not modified" response with the matching etag.
func notModifiedResult(etag string) *mcp.CallToolResult {
	result, _ := mcp.NewToolResultJSON(map[string]any{
		"not_modified": true,
		"etag":         etag,
	})
	return result
}

// withETag adds an etag field to a map result and returns the JSON tool result.
func withETag(data map[string]any) (*mcp.CallToolResult, error) {
	etag := computeETag(data)
	data["etag"] = etag
	return mcp.NewToolResultJSON(data)
}

// computePackRoot derives an order-insensitive content hash over the
// symbols a smart_context call selected — its "pack root". Unlike
// computeETag it ignores the incidental ordering of result lists (the
// search rerank can reorder an otherwise-unchanged pack), so a
// repeated call on unchanged code yields the same root and the
// if_none_match dedup fires reliably.
//
// Each symbol contributes its ID, start line, and source — all
// stable: the source is read from disk and the line is set at index
// time. Derived metadata (signatures) is deliberately excluded, since
// it can be re-enriched between calls. The root therefore changes when
// the set of selected symbols changes, when one moves, or when its
// source does.
func computePackRoot(result map[string]any) string {
	var items []string
	add := func(e map[string]any) {
		id, _ := e["id"].(string)
		body, _ := e["source"].(string)
		line := ""
		switch v := e["start_line"].(type) {
		case int:
			line = strconv.Itoa(v)
		case float64:
			line = strconv.Itoa(int(v))
		}
		items = append(items, id+"\x1f"+line+"\x1f"+body)
	}
	// A graded response carries its symbols (with source) in the
	// manifest; a flat one carries them in relevant_symbols. Hash
	// whichever is present so the same symbol is never counted twice.
	if mani, ok := result["context_manifest"].(map[string]any); ok {
		if entries, ok := mani["entries"].([]map[string]any); ok {
			for _, e := range entries {
				add(e)
			}
		}
	} else if syms, ok := result["relevant_symbols"].([]map[string]any); ok {
		for _, e := range syms {
			add(e)
		}
	}
	sort.Strings(items)

	h := sha256.New()
	if task, ok := result["task"].(string); ok {
		h.Write([]byte(task))
	}
	h.Write([]byte{0})
	for _, it := range items {
		h.Write([]byte(it))
		h.Write([]byte{0})
	}
	for _, key := range []string{"files_to_edit", "related_test_files"} {
		if list, ok := result[key].([]string); ok {
			cp := append([]string(nil), list...)
			sort.Strings(cp)
			for _, s := range cp {
				h.Write([]byte(s))
				h.Write([]byte{0})
			}
		}
		h.Write([]byte{1})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
