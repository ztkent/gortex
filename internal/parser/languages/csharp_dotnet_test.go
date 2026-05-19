package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestDetectDotNetSurfaces_DIRegistrations(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Startup.cs", Kind: graph.KindFile, FilePath: "Startup.cs"}},
	}
	src := []byte(`public void ConfigureServices(IServiceCollection services) {
    services.AddScoped<IUserRepo, UserRepo>();
    services.AddSingleton<ILogger>();
    builder.Services.AddTransient<IFoo, Foo>();
}`)
	detectDotNetSurfaces(src, result)

	regs, _ := result.Nodes[0].Meta["di_registrations"].([]string)
	require.Len(t, regs, 3)
	require.Contains(t, regs, "scoped IUserRepo -> UserRepo")
	require.Contains(t, regs, "singleton ILogger")
	require.Contains(t, regs, "transient IFoo -> Foo")
}

func TestDetectDotNetSurfaces_COMInterop(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Com.cs", Kind: graph.KindFile, FilePath: "Com.cs"}},
	}
	src := []byte(`[ComImport]
[Guid("00000000-0000-0000-0000-000000000000")]
interface IShellLink {}`)
	detectDotNetSurfaces(src, result)
	require.Equal(t, true, result.Nodes[0].Meta["com_interop"])
}

func TestDetectDotNetSurfaces_PlainFile(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{ID: "Plain.cs", Kind: graph.KindFile, FilePath: "Plain.cs"}},
	}
	detectDotNetSurfaces([]byte(`public class Plain { void M() {} }`), result)
	require.Nil(t, result.Nodes[0].Meta) // nothing to stamp
}
