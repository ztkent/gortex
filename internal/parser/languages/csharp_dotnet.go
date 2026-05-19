package languages

import (
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// diRegistrationRe matches a .NET dependency-injection registration —
// services.AddScoped<IFoo, Foo>(), AddSingleton<IBar>(),
// AddTransient<…>(), AddHostedService<…>().
var diRegistrationRe = regexp.MustCompile(
	`\.Add(Scoped|Singleton|Transient|HostedService)\s*<\s*([\w.]+)\s*(?:,\s*([\w.]+)\s*)?>`)

// comInteropRe matches a COM / native-interop marker attribute.
var comInteropRe = regexp.MustCompile(
	`\[\s*(?:ComImport|ComVisible|Guid|DllImport|InterfaceType|ClassInterface|ComSourceInterfaces)\b`)

// detectDotNetSurfaces stamps the C# file node with the two .NET
// surfaces a tree-sitter symbol walk does not otherwise expose:
// dependency-injection registrations (Meta["di_registrations"]) and a
// COM / native-interop flag (Meta["com_interop"]). Both are core to
// understanding a .NET service's wiring and unmanaged boundary.
func detectDotNetSurfaces(src []byte, result *parser.ExtractionResult) {
	var file *graph.Node
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile {
			file = n
			break
		}
	}
	if file == nil {
		return
	}
	text := string(src)

	var regs []string
	seen := map[string]bool{}
	for _, m := range diRegistrationRe.FindAllStringSubmatch(text, -1) {
		entry := strings.ToLower(m[1]) + " " + m[2]
		if m[3] != "" {
			entry += " -> " + m[3]
		}
		if !seen[entry] {
			seen[entry] = true
			regs = append(regs, entry)
		}
	}
	hasCOM := comInteropRe.MatchString(text)
	if len(regs) == 0 && !hasCOM {
		return
	}
	if file.Meta == nil {
		file.Meta = map[string]any{}
	}
	if len(regs) > 0 {
		sort.Strings(regs)
		file.Meta["di_registrations"] = regs
	}
	if hasCOM {
		file.Meta["com_interop"] = true
	}
}
