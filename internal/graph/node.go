package graph

type NodeKind string

const (
	KindFile      NodeKind = "file"
	KindPackage   NodeKind = "package"
	KindFunction  NodeKind = "function"
	KindMethod    NodeKind = "method"
	KindType      NodeKind = "type"
	KindInterface NodeKind = "interface"
	KindVariable  NodeKind = "variable"
	KindImport    NodeKind = "import"
	KindContract  NodeKind = "contract"
)

var validNodeKinds = map[NodeKind]bool{
	KindFile: true, KindPackage: true, KindFunction: true,
	KindMethod: true, KindType: true, KindInterface: true,
	KindVariable: true, KindImport: true, KindContract: true,
}

type Node struct {
	ID        string   `json:"id"`
	Kind      NodeKind `json:"kind"`
	Name      string   `json:"name"`
	QualName  string   `json:"qual_name,omitempty"`
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"`
	// EndLine is omitted when zero — File-kind nodes don't have ranges.
	EndLine    int            `json:"end_line,omitempty"`
	Language   string         `json:"language"`
	Meta       map[string]any `json:"meta,omitempty"`
	RepoPrefix string         `json:"repo_prefix,omitempty"`
	// WorkspaceID is the hard graph boundary slug. Two nodes with
	// different WorkspaceIDs are not allowed to be matched as contract
	// provider/consumer pairs and queries scope by it by default.
	// Defaults at warmup time to the per-repo `.gortex.yaml::workspace`
	// setting; falls back to RepoPrefix when no workspace is declared
	// (so old configs keep working) and to "" only for snapshot
	// records written before the field existed (gob decodes unknown
	// fields as zero — warmup backfills these from config).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID is the soft sub-boundary inside a workspace. One
	// project per repo by default; monorepos can declare projects[] in
	// .gortex.yaml. Contract pairing is bounded to a single
	// (workspace_id, project_id); cross-project contracts become orphans.
	// Defaults to the repo name when no projects[] mapping matches.
	ProjectID string `json:"project_id,omitempty"`
}

// Brief returns a compact representation with only the fields needed for listing.
func (n *Node) Brief() map[string]any {
	b := map[string]any{
		"id":         n.ID,
		"name":       n.Name,
		"kind":       n.Kind,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
	if n.RepoPrefix != "" {
		b["repo_prefix"] = n.RepoPrefix
	}
	if n.WorkspaceID != "" {
		b["workspace_id"] = n.WorkspaceID
	}
	if n.ProjectID != "" {
		b["project_id"] = n.ProjectID
	}
	return b
}

func ValidNodeKind(k NodeKind) bool {
	return validNodeKinds[k]
}
