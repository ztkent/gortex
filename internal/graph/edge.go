package graph

type EdgeKind string

const (
	EdgeImports      EdgeKind = "imports"
	EdgeDefines      EdgeKind = "defines"
	EdgeCalls        EdgeKind = "calls"
	EdgeInstantiates EdgeKind = "instantiates"
	EdgeImplements   EdgeKind = "implements"
	EdgeExtends      EdgeKind = "extends"
	EdgeReferences   EdgeKind = "references"
	EdgeMemberOf     EdgeKind = "member_of"
)

type Edge struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Kind       EdgeKind `json:"kind"`
	FilePath   string   `json:"file_path"`
	Line       int      `json:"line"`
	Confidence float64  `json:"confidence,omitempty"`
}
