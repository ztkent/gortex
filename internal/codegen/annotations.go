package codegen

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// annotationGen describes the codegen tool an annotation implies and
// the member kinds it produces — members that exist after compilation
// but never appear in source, so the indexer cannot see them.
type annotationGen struct {
	tool    string
	members []string
}

// annotationGenerators maps a generation-implying annotation (Java
// Lombok, MapStruct, Kotlin compiler plugins) to the tool plus the
// members it synthesises. Keyed by the bare annotation name — no `@`,
// no package qualifier.
var annotationGenerators = map[string]annotationGen{
	// Lombok.
	"Data":                    {"lombok", []string{"getters", "setters", "equals_hashcode", "tostring", "required_args_constructor"}},
	"Value":                   {"lombok", []string{"getters", "equals_hashcode", "tostring", "all_args_constructor"}},
	"Getter":                  {"lombok", []string{"getters"}},
	"Setter":                  {"lombok", []string{"setters"}},
	"Builder":                 {"lombok", []string{"builder"}},
	"SuperBuilder":            {"lombok", []string{"builder"}},
	"AllArgsConstructor":      {"lombok", []string{"all_args_constructor"}},
	"NoArgsConstructor":       {"lombok", []string{"no_args_constructor"}},
	"RequiredArgsConstructor": {"lombok", []string{"required_args_constructor"}},
	"EqualsAndHashCode":       {"lombok", []string{"equals_hashcode"}},
	"ToString":                {"lombok", []string{"tostring"}},
	"Slf4j":                   {"lombok", []string{"logger"}},
	// MapStruct — generates the <Mapper>Impl implementation class.
	"Mapper": {"mapstruct", []string{"mapper_impl"}},
	// Kotlin compiler plugins (KAPT / kotlin-parcelize).
	"Parcelize": {"kapt", []string{"parcelable"}},
	// CommunityToolkit.Mvvm .NET source generators: [ObservableProperty]
	// on a field generates the public property + change notification;
	// [RelayCommand] on a method generates an ICommand property.
	"ObservableProperty": {"mvvm_toolkit", []string{"observable_property"}},
	"RelayCommand":       {"mvvm_toolkit", []string{"relay_command"}},
}

// AnnotationGeneratedStats reports what MarkAnnotatedGenerated did.
type AnnotationGeneratedStats struct {
	NodesMarked int
	EdgesAdded  int
}

// normalizeAnnotationName strips a leading `@` and any package
// qualifier, leaving the bare annotation name.
func normalizeAnnotationName(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// MarkAnnotatedGenerated scans one file's extraction output for
// EdgeAnnotated edges that point at a generation-implying annotation
// (Lombok / MapStruct / Kotlin codegen) and stamps each annotated
// symbol with codegen-visibility metadata: has_generated_members,
// codegen_tool, and the generated_members list. It returns the
// EdgeGeneratedBy edges the caller must append to result.Edges.
//
// This makes generated members visible without materialising them: a
// `@Data` class is flagged as carrying Lombok-generated accessors, so
// dead-code analysis and agents know the fields are not unused and
// that a missing getName() is compiler output, not a bug.
func MarkAnnotatedGenerated(nodes []*graph.Node, edges []*graph.Edge) (extra []*graph.Edge, stats AnnotationGeneratedStats) {
	if len(edges) == 0 {
		return nil, stats
	}
	annoName := map[string]string{} // annotation node ID → bare name
	byID := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
		if n.Meta != nil {
			if k, _ := n.Meta["kind"].(string); k == "annotation" {
				annoName[n.ID] = normalizeAnnotationName(n.Name)
			}
		}
	}
	seenEdge := map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		gen, ok := annotationGenerators[annoName[e.To]]
		if !ok {
			continue
		}
		host := byID[e.From]
		if host == nil {
			continue
		}
		if host.Meta == nil {
			host.Meta = map[string]any{}
		}
		host.Meta["has_generated_members"] = true
		host.Meta["codegen_tool"] = gen.tool
		host.Meta["generated_members"] = mergeStringSet(host.Meta["generated_members"], gen.members)
		stats.NodesMarked++

		key := e.From + "\x00" + gen.tool
		if !seenEdge[key] {
			seenEdge[key] = true
			extra = append(extra, &graph.Edge{
				From:     e.From,
				To:       "external::generator-tool:" + gen.tool,
				Kind:     graph.EdgeGeneratedBy,
				FilePath: host.FilePath,
				Origin:   graph.OriginASTResolved,
				Meta:     map[string]any{"tool": gen.tool},
			})
			stats.EdgesAdded++
		}
	}
	return extra, stats
}

// mergeStringSet merges add into an existing []string-typed meta value,
// returning a sorted, de-duplicated slice.
func mergeStringSet(existing any, add []string) []string {
	set := map[string]bool{}
	if cur, ok := existing.([]string); ok {
		for _, s := range cur {
			set[s] = true
		}
	}
	for _, s := range add {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
