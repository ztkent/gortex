package languages

import (
	"encoding/xml"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
)

// DotNetProjectExtractor ingests .NET solution (.sln) and project
// (.csproj/.fsproj/.vbproj) files into the graph. It does NOT parse C#
// source — that is the tree-sitter C# extractor's job (with
// detectDotNetSurfaces stamping the .cs metadata). This extractor
// builds the *build graph*: which projects a solution groups, how
// projects depend on each other (ProjectReference / solution
// ProjectDependencies), and which NuGet packages a project pulls in
// (PackageReference → KindModule, ecosystem "nuget").
//
// File nodes are keyed by their repo-relative path so a ProjectReference
// emitted here lines up with the file node a later .csproj index pass
// produces — matching IDs is what stitches the cross-project edges
// together; no stub bookkeeping is required.
type DotNetProjectExtractor struct{}

// NewDotNetProjectExtractor constructs a DotNetProjectExtractor.
func NewDotNetProjectExtractor() *DotNetProjectExtractor { return &DotNetProjectExtractor{} }

func (e *DotNetProjectExtractor) Language() string { return "dotnet" }

func (e *DotNetProjectExtractor) Extensions() []string {
	return []string{".sln", ".csproj", ".fsproj", ".vbproj"}
}

func (e *DotNetProjectExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".sln":
		return e.extractSolution(filePath, src), nil
	default:
		// .csproj / .fsproj / .vbproj — all share the MSBuild XML shape.
		return e.extractProject(filePath, src), nil
	}
}

// slnProjectRe matches a Visual Studio solution Project declaration:
//
//	Project("{FAE04EC0-...}") = "MyApp", "src\MyApp\MyApp.csproj", "{GUID}"
//
// Capture groups: 1=project type GUID, 2=project name, 3=relative path,
// 4=project GUID.
var slnProjectRe = regexp.MustCompile(
	`(?i)^\s*Project\("\{([^}]*)\}"\)\s*=\s*"([^"]*)",\s*"([^"]*)",\s*"\{([^}]*)\}"`)

// slnDependencyRe matches a single dependency entry inside a
// ProjectSection(ProjectDependencies) block: `{GUID1} = {GUID2}`.
var slnDependencyRe = regexp.MustCompile(
	`^\s*\{([^}]*)\}\s*=\s*\{([^}]*)\}`)

// extractSolution parses a .sln file line by line. The .sln is an
// INI/text format; we capture each Project(...) declaration as a file
// node for the referenced project and wire ProjectDependencies blocks
// into EdgeDependsOn edges between project file nodes.
func (e *DotNetProjectExtractor) extractSolution(filePath string, src []byte) *parser.ExtractionResult {
	result := &parser.ExtractionResult{}
	slnDir := filepath.Dir(filePath)

	lines := strings.Split(string(src), "\n")
	slnNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filepath.Base(filePath),
		FilePath:  filePath,
		Language:  "dotnet",
		StartLine: 1,
		EndLine:   len(lines),
		Meta:      map[string]any{"kind": "dotnet_solution"},
	}
	result.Nodes = append(result.Nodes, slnNode)

	// guid (upper-cased) -> resolved repo-relative project file path.
	guidToPath := make(map[string]string)
	// projects we have already emitted a node for, keyed by resolved path.
	seenProject := make(map[string]bool)

	// First pass: collect every Project(...) declaration so the GUID map
	// is complete before we resolve ProjectDependencies (which may
	// reference a project declared later in the file).
	type pendingDep struct{ owner, dep string } // owner/dep are GUIDs (upper-cased)
	var deps []pendingDep

	var currentOwnerGUID string // GUID of the Project whose section we're in
	inDepSection := false

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Project("):
			m := slnProjectRe.FindStringSubmatch(line)
			if m == nil {
				currentOwnerGUID = ""
				continue
			}
			typeGUID := m[1]
			name := m[2]
			rel := m[3]
			projGUID := strings.ToUpper(m[4])
			currentOwnerGUID = projGUID

			// Solution folders use the well-known type GUID
			// {2150E333-8FDC-42A3-9474-1A3956D46DE8} and a virtual path
			// that is not a real file. Skip anything whose path doesn't
			// look like a project file.
			if !looksLikeProjectPath(rel) {
				continue
			}
			resolved := resolveDotNetPath(slnDir, rel)
			guidToPath[projGUID] = resolved
			if !seenProject[resolved] {
				seenProject[resolved] = true
				result.Nodes = append(result.Nodes, &graph.Node{
					ID:        resolved,
					Kind:      graph.KindFile,
					Name:      name,
					FilePath:  resolved,
					Language:  "dotnet",
					StartLine: i + 1,
					EndLine:   i + 1,
					Meta: map[string]any{
						"project_guid": projGUID,
						"project_type": typeGUID,
						"kind":         "dotnet_project",
					},
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From:     slnNode.ID,
					To:       resolved,
					Kind:     graph.EdgeDefines,
					FilePath: filePath,
					Line:     i + 1,
					Origin:   graph.OriginASTResolved,
				})
			}
		case strings.HasPrefix(line, "EndProject"):
			currentOwnerGUID = ""
			inDepSection = false
		case strings.HasPrefix(line, "ProjectSection(ProjectDependencies)"):
			inDepSection = true
		case strings.HasPrefix(line, "EndProjectSection"):
			inDepSection = false
		default:
			if inDepSection && currentOwnerGUID != "" {
				dm := slnDependencyRe.FindStringSubmatch(line)
				if dm == nil {
					continue
				}
				// Inside a ProjectDependencies section the line
				// `{GUID1} = {GUID2}` means the owning project depends on
				// GUID2 (GUID1 is conventionally the same as the owner).
				deps = append(deps, pendingDep{
					owner: currentOwnerGUID,
					dep:   strings.ToUpper(dm[2]),
				})
			}
		}
	}

	// Second pass: resolve GUID dependencies into file-node edges now
	// that guidToPath is complete.
	for _, d := range deps {
		from, ok1 := guidToPath[d.owner]
		to, ok2 := guidToPath[d.dep]
		if !ok1 || !ok2 || from == to {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       to,
			Kind:     graph.EdgeDependsOn,
			FilePath: filePath,
			Origin:   graph.OriginASTResolved,
		})
	}

	return result
}

// msbuildProject is the minimal MSBuild project shape we read. Both
// SDK-style and legacy .csproj/.fsproj/.vbproj files share these
// element names; we ignore everything we don't model.
type msbuildProject struct {
	XMLName        xml.Name `xml:"Project"`
	PropertyGroups []struct {
		TargetFramework  string `xml:"TargetFramework"`
		TargetFrameworks string `xml:"TargetFrameworks"`
	} `xml:"PropertyGroup"`
	ItemGroups []struct {
		ProjectReferences []struct {
			Include string `xml:"Include,attr"`
		} `xml:"ProjectReference"`
		PackageReferences []struct {
			Include string `xml:"Include,attr"`
			Version string `xml:"Version,attr"`
		} `xml:"PackageReference"`
	} `xml:"ItemGroup"`
}

// extractProject parses an MSBuild project file. It emits the project's
// own file node (with target-framework meta), EdgeDependsOn edges to
// referenced sibling projects, and KindModule nodes + EdgeDependsOnModule
// edges for NuGet PackageReferences.
func (e *DotNetProjectExtractor) extractProject(filePath string, src []byte) *parser.ExtractionResult {
	result := &parser.ExtractionResult{}
	projDir := filepath.Dir(filePath)

	var proj msbuildProject
	// Malformed project XML is best-effort: emit the file node so the
	// project still appears in the graph, but skip references.
	parseErr := xml.Unmarshal(src, &proj)

	// Collect the target framework(s) for meta.
	var frameworks []string
	if parseErr == nil {
		for _, pg := range proj.PropertyGroups {
			if tf := strings.TrimSpace(pg.TargetFramework); tf != "" {
				frameworks = append(frameworks, tf)
			}
			if tfs := strings.TrimSpace(pg.TargetFrameworks); tfs != "" {
				for _, f := range strings.Split(tfs, ";") {
					if f = strings.TrimSpace(f); f != "" {
						frameworks = append(frameworks, f)
					}
				}
			}
		}
	}

	fileMeta := map[string]any{"kind": "dotnet_project"}
	if len(frameworks) > 0 {
		fileMeta["target_framework"] = strings.Join(frameworks, ";")
	}
	fileNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filepath.Base(filePath),
		FilePath:  filePath,
		Language:  "dotnet",
		StartLine: 1,
		EndLine:   1,
		Meta:      fileMeta,
	}
	result.Nodes = append(result.Nodes, fileNode)
	if parseErr != nil {
		return result
	}

	seenRef := make(map[string]bool)
	seenModule := make(map[string]bool)
	for _, ig := range proj.ItemGroups {
		for _, pr := range ig.ProjectReferences {
			rel := strings.TrimSpace(pr.Include)
			if rel == "" {
				continue
			}
			target := resolveDotNetPath(projDir, rel)
			if target == filePath || seenRef[target] {
				continue
			}
			seenRef[target] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fileNode.ID,
				To:       target,
				Kind:     graph.EdgeDependsOn,
				FilePath: filePath,
				Origin:   graph.OriginASTResolved,
			})
		}
		for _, pkg := range ig.PackageReferences {
			name := strings.TrimSpace(pkg.Include)
			if name == "" {
				continue
			}
			version := strings.TrimSpace(pkg.Version)
			id := modules.ModuleNodeID("nuget", name, version)
			if !seenModule[id] {
				seenModule[id] = true
				meta := map[string]any{
					"ecosystem": "nuget",
					"path":      name,
					"version":   version,
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID:       id,
					Kind:     graph.KindModule,
					Name:     name,
					FilePath: filePath,
					Language: "dotnet",
					Meta:     meta,
				})
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fileNode.ID,
				To:       id,
				Kind:     graph.EdgeDependsOnModule,
				FilePath: filePath,
				Origin:   graph.OriginASTResolved,
			})
		}
	}

	return result
}

// resolveDotNetPath converts an MSBuild/solution-relative path (which
// uses Windows backslashes) into a clean, forward-slashed path relative
// to baseDir. The result keys file nodes so a ProjectReference here and
// the referenced project's own index pass agree on the ID.
func resolveDotNetPath(baseDir, rel string) string {
	rel = strings.ReplaceAll(rel, `\`, "/")
	joined := filepath.ToSlash(filepath.Join(baseDir, rel))
	return filepath.ToSlash(filepath.Clean(joined))
}

// looksLikeProjectPath reports whether a solution Project(...) path
// points at a real project file (vs. a solution-folder virtual path,
// which is typically just the folder name with no extension).
func looksLikeProjectPath(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".csproj", ".fsproj", ".vbproj", ".proj", ".sqlproj", ".vcxproj", ".shproj":
		return true
	default:
		return false
	}
}

var _ parser.Extractor = (*DotNetProjectExtractor)(nil)
