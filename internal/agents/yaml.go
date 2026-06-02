package agents

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	yaml "gopkg.in/yaml.v3"
)

// MergeYAML is the YAML cousin of MergeJSON / MergeTOML, with one
// important difference: it preserves comments and structure. Hermes
// config (~/.hermes/config.yaml) is comment-rich and hand-edited, so
// a round-trip through map[string]any — which silently drops every
// comment and reorders keys — would mangle the user's file. Instead
// we decode into a *yaml.Node tree (which carries HeadComment /
// LineComment / FootComment on every node) and re-encode it, so an
// idempotent re-run is a no-op and a real merge touches only the keys
// we add.
//
// mutate receives the top-level mapping node (created empty for a new
// or empty file) and reports whether it changed anything. The agents
// package ships node helpers — YAMLMapValue / YAMLSetMapValue /
// YAMLScalar / UpsertYAMLMapEntry — so callers don't hand-walk the
// Content slice.
//
// Malformed YAML is preserved as a .bak sibling before we overwrite,
// same policy as MergeJSON / MergeTOML. A valid-but-non-mapping
// top-level document (a bare scalar or sequence — never a real config
// file) is also backed up before we replace it with a fresh mapping.
func MergeYAML(w io.Writer, path string, mutate func(root *yaml.Node, existed bool) (changed bool, err error), opts ApplyOpts) (FileAction, error) {
	existed := false
	var backupPath string
	var doc yaml.Node

	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if len(bytes.TrimSpace(data)) > 0 {
			if err := yaml.Unmarshal(data, &doc); err != nil {
				backupPath = path + ".bak"
				_ = os.WriteFile(backupPath, data, 0o644)
				doc = yaml.Node{}
			} else if !documentHasMappingRoot(&doc) {
				// Valid YAML but not a top-level mapping (a bare list
				// or scalar). We can't safely splice mcp_servers into
				// that, so preserve it and start fresh.
				backupPath = path + ".bak"
				_ = os.WriteFile(backupPath, data, 0o644)
				doc = yaml.Node{}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}

	root := documentRoot(&doc)

	changed, err := mutate(root, existed)
	if err != nil {
		return FileAction{}, err
	}
	if !changed {
		return FileAction{Path: path, Action: ActionSkip, Reason: "already-configured"}, nil
	}

	keys := yamlTopLevelKeys(root)

	if opts.DryRun {
		action := ActionWouldCreate
		if existed {
			action = ActionWouldMerge
		}
		return FileAction{Path: path, Action: action, Keys: keys}, nil
	}

	out, err := marshalYAMLDocument(&doc)
	if err != nil {
		return FileAction{}, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := AtomicWriteFile(path, out, 0o644); err != nil {
		return FileAction{}, err
	}

	action := ActionCreate
	if existed {
		action = ActionMerge
	}
	if backupPath != "" {
		logf(w, "[gortex init] %s was malformed YAML; backup saved to %s", path, backupPath)
	}
	logf(w, "[gortex init] %s %s", actionVerb(action), path)
	return FileAction{Path: path, Action: action, Keys: keys}, nil
}

// documentHasMappingRoot reports whether a freshly-unmarshaled
// document node carries a mapping at its root — the only shape we can
// safely merge into.
func documentHasMappingRoot(doc *yaml.Node) bool {
	return doc.Kind == yaml.DocumentNode &&
		len(doc.Content) > 0 &&
		doc.Content[0].Kind == yaml.MappingNode
}

// documentRoot normalises doc so it is a DocumentNode wrapping a
// MappingNode and returns that mapping. A zero-value node (empty /
// absent file) is turned into an empty document so the caller always
// gets a writable mapping back.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if documentHasMappingRoot(doc) {
		return doc.Content[0]
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	doc.Kind = yaml.DocumentNode
	doc.Content = []*yaml.Node{mapping}
	return mapping
}

// marshalYAMLDocument renders a document node back to bytes with the
// 2-space indent Hermes (and most hand-written YAML) uses, preserving
// the comments captured during Unmarshal.
func marshalYAMLDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlTopLevelKeys returns the key names of a mapping node, in file
// order. Used to populate FileAction.Keys for the --json report.
func yamlTopLevelKeys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}
	return out
}

// YAMLMapValue returns the value node stored under key in a mapping
// node, or nil when the key is absent (or m isn't a mapping). YAML
// mappings are stored as a flat [k0, v0, k1, v1, …] Content slice;
// this hides that layout from callers.
func YAMLMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// YAMLSetMapValue sets key=val in a mapping node, replacing an
// existing value in place (so its leading comment survives) or
// appending a new key/value pair.
func YAMLSetMapValue(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, YAMLScalar(key), val)
}

// YAMLScalar builds a plain string scalar node. Callers that need
// non-string scalars (ints, bools) construct yaml.Node literals with
// the matching tag directly.
func YAMLScalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// UpsertYAMLMapEntry ensures root[outerKey][innerKey] = entry, where
// outerKey's value is a nested mapping (created when absent). Returns
// true when the tree was modified, false when innerKey already exists
// and force is off — the idempotent-re-run signal MergeYAML turns
// into an "already-configured" skip.
//
// This is the YAML analogue of UpsertMCPServer: Hermes stores MCP
// servers under the snake_case `mcp_servers` map rather than the
// camelCase `mcpServers` every JSON client uses.
func UpsertYAMLMapEntry(root *yaml.Node, outerKey, innerKey string, entry *yaml.Node, force bool) (changed bool) {
	outer := YAMLMapValue(root, outerKey)
	if outer == nil || outer.Kind != yaml.MappingNode {
		outer = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		YAMLSetMapValue(root, outerKey, outer)
	}
	if existing := YAMLMapValue(outer, innerKey); existing != nil && !force {
		return false
	}
	YAMLSetMapValue(outer, innerKey, entry)
	return true
}
