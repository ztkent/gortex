package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// findNode returns the first node with the given ID, or nil.
func dnFindNode(nodes []*graph.Node, id string) *graph.Node {
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// hasEdge reports whether an edge of the given kind from->to exists.
func dnHasEdge(edges []*graph.Edge, kind graph.EdgeKind, from, to string) bool {
	for _, e := range edges {
		if e.Kind == kind && e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestDotNetProjectExtractor_Csproj(t *testing.T) {
	const filePath = "src/MyApp/MyApp.csproj"
	src := []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <Nullable>enable</Nullable>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="..\Core\Core.csproj" />
    <PackageReference Include="Newtonsoft.Json" Version="13.0.1" />
    <PackageReference Include="Serilog" />
  </ItemGroup>
</Project>`)

	e := NewDotNetProjectExtractor()
	if got := e.Language(); got != "dotnet" {
		t.Fatalf("Language() = %q, want %q", got, "dotnet")
	}
	res, err := e.Extract(filePath, src)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	// File node with target_framework meta.
	fileNode := dnFindNode(res.Nodes, filePath)
	if fileNode == nil {
		t.Fatalf("expected file node with ID %q", filePath)
	}
	if fileNode.Kind != graph.KindFile {
		t.Fatalf("file node kind = %q, want %q", fileNode.Kind, graph.KindFile)
	}
	if fileNode.Language != "dotnet" {
		t.Fatalf("file node language = %q, want %q", fileNode.Language, "dotnet")
	}
	if tf, _ := fileNode.Meta["target_framework"].(string); tf != "net8.0" {
		t.Fatalf("target_framework meta = %q, want %q", tf, "net8.0")
	}
	if k, _ := fileNode.Meta["kind"].(string); k != "dotnet_project" {
		t.Fatalf("file node meta kind = %q, want %q", k, "dotnet_project")
	}

	// ProjectReference resolves `..\Core\Core.csproj` relative to
	// `src/MyApp` → `src/Core/Core.csproj` (backslash -> forward slash).
	const wantRef = "src/Core/Core.csproj"
	if !dnHasEdge(res.Edges, graph.EdgeDependsOn, filePath, wantRef) {
		t.Fatalf("expected EdgeDependsOn %q -> %q; edges: %+v", filePath, wantRef, res.Edges)
	}

	// PackageReference with version → KindModule node + EdgeDependsOnModule.
	const wantModule = "module::nuget:Newtonsoft.Json@13.0.1"
	mod := dnFindNode(res.Nodes, wantModule)
	if mod == nil {
		t.Fatalf("expected module node with ID %q", wantModule)
	}
	if mod.Kind != graph.KindModule {
		t.Fatalf("module node kind = %q, want %q", mod.Kind, graph.KindModule)
	}
	if eco, _ := mod.Meta["ecosystem"].(string); eco != "nuget" {
		t.Fatalf("module ecosystem = %q, want %q", eco, "nuget")
	}
	if !dnHasEdge(res.Edges, graph.EdgeDependsOnModule, filePath, wantModule) {
		t.Fatalf("expected EdgeDependsOnModule %q -> %q", filePath, wantModule)
	}

	// Versionless PackageReference uses the versionless module ID form.
	const wantVersionless = "module::nuget:Serilog"
	if dnFindNode(res.Nodes, wantVersionless) == nil {
		t.Fatalf("expected versionless module node with ID %q", wantVersionless)
	}
	if !dnHasEdge(res.Edges, graph.EdgeDependsOnModule, filePath, wantVersionless) {
		t.Fatalf("expected EdgeDependsOnModule %q -> %q", filePath, wantVersionless)
	}
}

func TestDotNetProjectExtractor_Sln(t *testing.T) {
	const filePath = "MySolution.sln"
	// Two projects; App depends on Lib via a ProjectDependencies block.
	const appGUID = "11111111-1111-1111-1111-111111111111"
	const libGUID = "22222222-2222-2222-2222-222222222222"
	src := []byte(`Microsoft Visual Studio Solution File, Format Version 12.00
Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "App", "src\App\App.csproj", "{` + appGUID + `}"
	ProjectSection(ProjectDependencies) = postProject
		{` + libGUID + `} = {` + libGUID + `}
	EndProjectSection
EndProject
Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Lib", "src\Lib\Lib.csproj", "{` + libGUID + `}"
EndProject
Global
EndGlobal`)

	e := NewDotNetProjectExtractor()
	res, err := e.Extract(filePath, src)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	// Two project file nodes with resolved (backslash->slash) paths.
	const appPath = "src/App/App.csproj"
	const libPath = "src/Lib/Lib.csproj"
	for _, want := range []string{appPath, libPath} {
		n := dnFindNode(res.Nodes, want)
		if n == nil {
			t.Fatalf("expected project file node %q", want)
		}
		if n.Kind != graph.KindFile {
			t.Fatalf("project node %q kind = %q, want %q", want, n.Kind, graph.KindFile)
		}
		if g, _ := n.Meta["project_guid"].(string); g == "" {
			t.Fatalf("project node %q missing project_guid meta", want)
		}
	}

	// App project_guid should match (upper-cased).
	app := dnFindNode(res.Nodes, appPath)
	if g, _ := app.Meta["project_guid"].(string); g != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("App project_guid = %q, want upper-cased GUID", g)
	}

	// Solution file node defines both projects.
	if !dnHasEdge(res.Edges, graph.EdgeDefines, filePath, appPath) {
		t.Fatalf("expected EdgeDefines %q -> %q", filePath, appPath)
	}
	if !dnHasEdge(res.Edges, graph.EdgeDefines, filePath, libPath) {
		t.Fatalf("expected EdgeDefines %q -> %q", filePath, libPath)
	}

	// App depends on Lib (resolved via the GUID map).
	if !dnHasEdge(res.Edges, graph.EdgeDependsOn, appPath, libPath) {
		t.Fatalf("expected EdgeDependsOn %q -> %q; edges: %+v", appPath, libPath, res.Edges)
	}
}
