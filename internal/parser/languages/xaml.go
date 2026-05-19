package languages

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// XAMLExtractor extracts structure from XAML / AXAML markup — the
// declarative UI layer of WPF, UWP, MAUI, WinUI and Avalonia .NET
// apps. XAML is well-formed XML, so it is parsed with encoding/xml
// rather than tree-sitter.
//
// What it surfaces:
//   - the file node, stamped with x:Class (the code-behind type the
//     view is bound to);
//   - one node per x:Name'd control, so a control referenced from
//     code-behind resolves to a real graph node;
//   - {Binding …} expressions, recorded on the owning control so the
//     data-binding surface is queryable.
type XAMLExtractor struct{}

// NewXAMLExtractor constructs a XAMLExtractor.
func NewXAMLExtractor() *XAMLExtractor { return &XAMLExtractor{} }

func (e *XAMLExtractor) Language() string     { return "xaml" }
func (e *XAMLExtractor) Extensions() []string { return []string{".xaml", ".axaml"} }

// Extract parses XAML markup. A malformed document yields whatever was
// decoded before the error — never a hard failure.
func (e *XAMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "xaml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Strict = false
	rootSeen := false
	seenName := map[string]bool{}

	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break // EOF or malformed — keep what was decoded
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		line := 1 + bytes.Count(src[:clampOffset(dec.InputOffset(), len(src))], []byte{'\n'})

		if !rootSeen {
			rootSeen = true
			if cls := xamlAttr(se, "Class"); cls != "" {
				fileNode.Meta = map[string]any{"xaml_class": cls}
			}
		}

		name := xamlAttr(se, "Name")
		if name == "" || seenName[name] {
			continue
		}
		seenName[name] = true

		ctrl := &graph.Node{
			ID:        filePath + "::" + name,
			Kind:      graph.KindVariable,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "xaml",
			Meta:      map[string]any{"xaml_element": se.Name.Local},
		}
		var bindings []string
		for _, a := range se.Attr {
			if v := strings.TrimSpace(a.Value); strings.HasPrefix(v, "{Binding") {
				bindings = append(bindings, a.Name.Local+"="+v)
			}
		}
		if len(bindings) > 0 {
			ctrl.Meta["xaml_bindings"] = bindings
		}
		result.Nodes = append(result.Nodes, ctrl)
	}
	return result, nil
}

// xamlAttr returns the value of the attribute with the given local
// name (namespace-agnostic, so both `x:Name` and `Name` match "Name").
func xamlAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

func clampOffset(off int64, n int) int64 {
	if off > int64(n) {
		return int64(n)
	}
	if off < 0 {
		return 0
	}
	return off
}
