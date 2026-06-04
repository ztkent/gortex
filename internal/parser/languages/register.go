package languages

import "github.com/zzet/gortex/internal/parser"

// RegisterAll registers all available language extractors.
func RegisterAll(reg *parser.Registry) {
	reg.Register(NewGoExtractor())
	reg.Register(NewTypeScriptExtractor())
	reg.Register(NewJavaScriptExtractor())
	reg.Register(NewPythonExtractor())
	reg.Register(NewRustExtractor())
	reg.Register(NewJavaExtractor())
	reg.Register(NewRubyExtractor())
	reg.Register(NewElixirExtractor())
	reg.Register(NewCExtractor())
	reg.Register(NewCppExtractor())
	reg.Register(NewHTMLExtractor())
	reg.Register(NewCSSExtractor())
	reg.Register(NewSQLExtractor())
	reg.Register(NewKotlinExtractor())
	reg.Register(NewSwiftExtractor())
	reg.Register(NewPHPExtractor())
	reg.Register(NewScalaExtractor())
	reg.Register(NewBashExtractor())
	reg.Register(NewProtobufExtractor())
	reg.Register(NewYAMLExtractor())
	reg.Register(NewTOMLExtractor())
	reg.Register(NewHCLExtractor())
	reg.Register(NewDockerfileExtractor())
	reg.Register(NewCSharpExtractor())
	reg.Register(NewXAMLExtractor())
	// .NET solution / project files — build-graph ingestion (.sln
	// project grouping, .csproj/.fsproj/.vbproj ProjectReference +
	// PackageReference). Registered before registerForestLanguages so
	// it owns .csproj/.sln over any generic forest XML grammar.
	reg.Register(NewDotNetProjectExtractor())
	// MyBatis and Spring both share the .xml extension with the generic
	// XML extractor; they are registered before registerForestLanguages
	// (which re-claims .xml for "xml" as the default) and routed only for
	// their respective documents via the content sniff in
	// detect_content.go.
	reg.Register(NewMyBatisExtractor())
	reg.Register(NewSpringContextExtractor())
	reg.Register(NewMarkdownExtractor())
	reg.Register(NewQuartoExtractor())
	reg.Register(NewOrgModeExtractor())
	reg.Register(NewDartExtractor())
	reg.Register(NewOCamlExtractor())
	reg.Register(NewLuaExtractor())
	reg.Register(NewZigExtractor())
	reg.Register(NewHaskellExtractor())
	reg.Register(NewClojureExtractor())
	reg.Register(NewErlangExtractor())
	reg.Register(NewRExtractor())
	reg.Register(NewVerseExtractor())
	reg.Register(NewALExtractor())
	reg.Register(NewAutoHotkeyExtractor())
	reg.Register(NewAssemblyExtractor())
	reg.Register(NewGDScriptExtractor())
	reg.Register(NewNixExtractor())
	reg.Register(NewFortranExtractor())
	reg.Register(NewSolidityExtractor())
	reg.Register(NewFSharpExtractor())
	reg.Register(NewJuliaExtractor())
	reg.Register(NewTclExtractor())
	reg.Register(NewShaderExtractor())
	reg.Register(NewPerlExtractor())
	reg.Register(NewRakuExtractor())
	reg.Register(NewCrystalExtractor())
	reg.Register(NewNimExtractor())
	reg.Register(NewPascalExtractor())
	reg.Register(NewCobolExtractor())
	reg.Register(NewAdaExtractor())
	reg.Register(NewPowerShellExtractor())
	reg.Register(NewVimExtractor())
	reg.Register(NewEmacsLispExtractor())
	reg.Register(NewRacketExtractor())

	// Template engines
	reg.Register(NewBladeExtractor())
	reg.Register(NewEJSExtractor())
	reg.Register(NewHandlebarsExtractor())
	reg.Register(NewJinjaExtractor())
	reg.Register(NewTwigExtractor())
	reg.Register(NewERBExtractor())
	reg.Register(NewLiquidExtractor())
	reg.Register(NewPugExtractor())

	// Build / shell
	reg.Register(NewMakefileExtractor())
	reg.Register(NewCMakeExtractor())
	reg.Register(NewBatchExtractor())

	// Blockchain / smart-contract
	reg.Register(NewMoveExtractor())
	reg.Register(NewCairoExtractor())
	reg.Register(NewNoirExtractor())
	reg.Register(NewTactExtractor())
	reg.Register(NewBallerinaExtractor())

	// Scientific / enterprise
	reg.Register(NewApexExtractor())
	reg.Register(NewABAPExtractor())
	reg.Register(NewMatlabExtractor())
	reg.Register(NewMathematicaExtractor())
	reg.Register(NewSASExtractor())
	reg.Register(NewStataExtractor())

	// Emerging
	reg.Register(NewMojoExtractor())
	reg.Register(NewOdinExtractor())
	reg.Register(NewVlangExtractor())
	reg.Register(NewHareExtractor())
	reg.Register(NewCarbonExtractor())
	reg.Register(NewReScriptExtractor())
	reg.Register(NewGleamExtractor())

	// Legacy / JVM / data
	reg.Register(NewCoffeeScriptExtractor())
	reg.Register(NewActionScriptExtractor())
	reg.Register(NewDExtractor())
	reg.Register(NewValaExtractor())
	reg.Register(NewGroovyExtractor())
	reg.Register(NewJSONExtractor())

	// Notebooks
	reg.Register(NewJupyterExtractor())

	// Forest-backed (alexaandru/go-sitter-forest) — signature-only.
	// Each registration adds a brand-new language not covered by the
	// hand-written extractors above. The forest framework reads the
	// grammar's bundled tags.scm when present and falls back to a
	// generic node-kind walker otherwise.
	reg.Register(NewElmExtractor())
	registerForestLanguages(reg)

	// ObjC registered last so it wins the `.m` extension over Matlab.
	reg.Register(NewObjCExtractor())
}
