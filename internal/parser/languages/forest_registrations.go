package languages

import (
	// Phase 1 — frontend / templates / docs / IaC / shaders / functional
	"github.com/alexaandru/go-sitter-forest/agda"
	"github.com/alexaandru/go-sitter-forest/asciidoc"
	"github.com/alexaandru/go-sitter-forest/astro"
	"github.com/alexaandru/go-sitter-forest/beancount"
	"github.com/alexaandru/go-sitter-forest/bicep"
	"github.com/alexaandru/go-sitter-forest/c3"
	"github.com/alexaandru/go-sitter-forest/capnp"
	"github.com/alexaandru/go-sitter-forest/cuda"
	"github.com/alexaandru/go-sitter-forest/cue"
	"github.com/alexaandru/go-sitter-forest/dhall"
	"github.com/alexaandru/go-sitter-forest/djot"
	"github.com/alexaandru/go-sitter-forest/fennel"
	"github.com/alexaandru/go-sitter-forest/fish"
	"github.com/alexaandru/go-sitter-forest/gherkin"
	"github.com/alexaandru/go-sitter-forest/glsl"
	"github.com/alexaandru/go-sitter-forest/gotmpl"
	"github.com/alexaandru/go-sitter-forest/graphql"
	"github.com/alexaandru/go-sitter-forest/gren"
	"github.com/alexaandru/go-sitter-forest/hack"
	"github.com/alexaandru/go-sitter-forest/haml"
	"github.com/alexaandru/go-sitter-forest/haxe"
	"github.com/alexaandru/go-sitter-forest/hlsl"
	"github.com/alexaandru/go-sitter-forest/htmldjango"
	// htmlaskama and unison excluded: niche grammars with C source
	// emitting compile warnings (Apple Clang doesn't recognise some
	// of their pragmas). Re-enable once upstream cleans them up.
	"github.com/alexaandru/go-sitter-forest/hurl"
	"github.com/alexaandru/go-sitter-forest/idris"
	"github.com/alexaandru/go-sitter-forest/ispc"
	"github.com/alexaandru/go-sitter-forest/janet"
	"github.com/alexaandru/go-sitter-forest/jq"
	"github.com/alexaandru/go-sitter-forest/jsonnet"
	"github.com/alexaandru/go-sitter-forest/just"
	"github.com/alexaandru/go-sitter-forest/kcl"
	"github.com/alexaandru/go-sitter-forest/kdl"
	"github.com/alexaandru/go-sitter-forest/latex"
	"github.com/alexaandru/go-sitter-forest/ledger"
	"github.com/alexaandru/go-sitter-forest/mermaid"
	"github.com/alexaandru/go-sitter-forest/meson"
	"github.com/alexaandru/go-sitter-forest/mlir"
	"github.com/alexaandru/go-sitter-forest/nickel"
	"github.com/alexaandru/go-sitter-forest/norg"
	"github.com/alexaandru/go-sitter-forest/nu"
	"github.com/alexaandru/go-sitter-forest/pkl"
	"github.com/alexaandru/go-sitter-forest/pony"
	"github.com/alexaandru/go-sitter-forest/prisma"
	"github.com/alexaandru/go-sitter-forest/purescript"
	"github.com/alexaandru/go-sitter-forest/robot"
	"github.com/alexaandru/go-sitter-forest/roc"
	"github.com/alexaandru/go-sitter-forest/ron"
	"github.com/alexaandru/go-sitter-forest/slim"
	"github.com/alexaandru/go-sitter-forest/smithy"
	"github.com/alexaandru/go-sitter-forest/svelte"
	"github.com/alexaandru/go-sitter-forest/thrift"
	"github.com/alexaandru/go-sitter-forest/typst"
	"github.com/alexaandru/go-sitter-forest/vhdl"
	"github.com/alexaandru/go-sitter-forest/vue"
	"github.com/alexaandru/go-sitter-forest/wgsl"

	// Phase 2 — HW / build / config / DSL / docs / queries / data
	"github.com/alexaandru/go-sitter-forest/aiken"
	"github.com/alexaandru/go-sitter-forest/awk"
	"github.com/alexaandru/go-sitter-forest/bibtex"
	"github.com/alexaandru/go-sitter-forest/bitbake"
	"github.com/alexaandru/go-sitter-forest/caddy"
	"github.com/alexaandru/go-sitter-forest/cedar"
	"github.com/alexaandru/go-sitter-forest/cel"
	"github.com/alexaandru/go-sitter-forest/circom"
	"github.com/alexaandru/go-sitter-forest/clarity"
	"github.com/alexaandru/go-sitter-forest/commonlisp"
	"github.com/alexaandru/go-sitter-forest/cooklang"
	"github.com/alexaandru/go-sitter-forest/dataweave"
	"github.com/alexaandru/go-sitter-forest/dbml"
	"github.com/alexaandru/go-sitter-forest/desktop"
	"github.com/alexaandru/go-sitter-forest/devicetree"
	"github.com/alexaandru/go-sitter-forest/dot"
	"github.com/alexaandru/go-sitter-forest/dotenv"
	"github.com/alexaandru/go-sitter-forest/dtd"
	"github.com/alexaandru/go-sitter-forest/earthfile"
	"github.com/alexaandru/go-sitter-forest/editorconfig"
	"github.com/alexaandru/go-sitter-forest/effekt"
	"github.com/alexaandru/go-sitter-forest/eiffel"
	"github.com/alexaandru/go-sitter-forest/elvish"
	"github.com/alexaandru/go-sitter-forest/firrtl"
	"github.com/alexaandru/go-sitter-forest/gdshader"
	"github.com/alexaandru/go-sitter-forest/git_config"
	"github.com/alexaandru/go-sitter-forest/gitattributes"
	"github.com/alexaandru/go-sitter-forest/gitcommit"
	"github.com/alexaandru/go-sitter-forest/gitignore"
	"github.com/alexaandru/go-sitter-forest/glimmer"
	"github.com/alexaandru/go-sitter-forest/gn"
	"github.com/alexaandru/go-sitter-forest/gnuplot"
	"github.com/alexaandru/go-sitter-forest/godot_resource"
	"github.com/alexaandru/go-sitter-forest/gomod"
	"github.com/alexaandru/go-sitter-forest/gosum"
	"github.com/alexaandru/go-sitter-forest/gowork"
	"github.com/alexaandru/go-sitter-forest/gpg"
	"github.com/alexaandru/go-sitter-forest/gritql"
	"github.com/alexaandru/go-sitter-forest/heex"
	"github.com/alexaandru/go-sitter-forest/hjson"
	"github.com/alexaandru/go-sitter-forest/hocon"
	"github.com/alexaandru/go-sitter-forest/hyprlang"
	"github.com/alexaandru/go-sitter-forest/ini"
	"github.com/alexaandru/go-sitter-forest/jasmin"
	"github.com/alexaandru/go-sitter-forest/json5"
	"github.com/alexaandru/go-sitter-forest/jsonc"
	"github.com/alexaandru/go-sitter-forest/jule"
	"github.com/alexaandru/go-sitter-forest/kconfig"
	"github.com/alexaandru/go-sitter-forest/koka"
	"github.com/alexaandru/go-sitter-forest/kusto"
	"github.com/alexaandru/go-sitter-forest/linkerscript"
	"github.com/alexaandru/go-sitter-forest/llvm"
	"github.com/alexaandru/go-sitter-forest/luau"
	"github.com/alexaandru/go-sitter-forest/moonbit"
	"github.com/alexaandru/go-sitter-forest/motoko"
	"github.com/alexaandru/go-sitter-forest/mustache"
	"github.com/alexaandru/go-sitter-forest/nftables"
	"github.com/alexaandru/go-sitter-forest/ninja"
	"github.com/alexaandru/go-sitter-forest/ocamllex"
	"github.com/alexaandru/go-sitter-forest/passwd"
	"github.com/alexaandru/go-sitter-forest/pem"
	"github.com/alexaandru/go-sitter-forest/pgn"
	"github.com/alexaandru/go-sitter-forest/pioasm"
	"github.com/alexaandru/go-sitter-forest/plantuml"
	"github.com/alexaandru/go-sitter-forest/po"
	"github.com/alexaandru/go-sitter-forest/poe_filter"
	"github.com/alexaandru/go-sitter-forest/promql"
	"github.com/alexaandru/go-sitter-forest/properties"
	"github.com/alexaandru/go-sitter-forest/prql"
	"github.com/alexaandru/go-sitter-forest/psv"
	"github.com/alexaandru/go-sitter-forest/puppet"
	"github.com/alexaandru/go-sitter-forest/qbe"
	"github.com/alexaandru/go-sitter-forest/ql"
	"github.com/alexaandru/go-sitter-forest/quint"
	"github.com/alexaandru/go-sitter-forest/ralph"
	"github.com/alexaandru/go-sitter-forest/razor"
	"github.com/alexaandru/go-sitter-forest/rbs"
	"github.com/alexaandru/go-sitter-forest/rego"
	"github.com/alexaandru/go-sitter-forest/requirements"
	"github.com/alexaandru/go-sitter-forest/scfg"
	"github.com/alexaandru/go-sitter-forest/scheme"
	"github.com/alexaandru/go-sitter-forest/scss"
	"github.com/alexaandru/go-sitter-forest/sml"
	"github.com/alexaandru/go-sitter-forest/snakemake"
	"github.com/alexaandru/go-sitter-forest/soql"
	"github.com/alexaandru/go-sitter-forest/sosl"
	"github.com/alexaandru/go-sitter-forest/sourcepawn"
	"github.com/alexaandru/go-sitter-forest/sparql"
	"github.com/alexaandru/go-sitter-forest/ssh_config"
	"github.com/alexaandru/go-sitter-forest/starlark"
	"github.com/alexaandru/go-sitter-forest/strace"
	"github.com/alexaandru/go-sitter-forest/structurizr"
	"github.com/alexaandru/go-sitter-forest/superhtml"
	"github.com/alexaandru/go-sitter-forest/surrealql"
	"github.com/alexaandru/go-sitter-forest/sxhkdrc"
	"github.com/alexaandru/go-sitter-forest/systemverilog"
	"github.com/alexaandru/go-sitter-forest/templ"
	"github.com/alexaandru/go-sitter-forest/tera"
	"github.com/alexaandru/go-sitter-forest/textproto"
	"github.com/alexaandru/go-sitter-forest/tlaplus"
	"github.com/alexaandru/go-sitter-forest/tmux"
	"github.com/alexaandru/go-sitter-forest/todotxt"
	"github.com/alexaandru/go-sitter-forest/tsv"
	"github.com/alexaandru/go-sitter-forest/turtle"
	"github.com/alexaandru/go-sitter-forest/typespec"
	"github.com/alexaandru/go-sitter-forest/usd"
	"github.com/alexaandru/go-sitter-forest/vento"
	"github.com/alexaandru/go-sitter-forest/vrl"
	"github.com/alexaandru/go-sitter-forest/wing"
	"github.com/alexaandru/go-sitter-forest/wit"
	"github.com/alexaandru/go-sitter-forest/xml"
	"github.com/alexaandru/go-sitter-forest/yang"
	"github.com/alexaandru/go-sitter-forest/zeek"
	"github.com/alexaandru/go-sitter-forest/ziggy"
	"github.com/alexaandru/go-sitter-forest/ziggy_schema"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/forest"
)

// forestLang is one row in the registration table. Keeping it as a
// local type avoids cluttering the parser package with a forest-only
// concept.
type forestLang struct {
	name    string
	exts    []string
	getLang forest.GetLanguageFn
	getQry  forest.GetQueryFn
}

// forestLanguages enumerates every forest-backed signature-only
// extractor. Languages already covered by hand-written extractors
// are silently skipped at registration time by registerForestLanguages
// (name OR extension collision → skip), so this table is allowed to
// list overlapping rows; bespoke depth wins.
var forestLanguages = []forestLang{
	// Frontend / templating
	{"vue", []string{".vue"}, vue.GetLanguage, vue.GetQuery},
	{"svelte", []string{".svelte"}, svelte.GetLanguage, svelte.GetQuery},
	{"astro", []string{".astro"}, astro.GetLanguage, astro.GetQuery},
	{"htmldjango", []string{".djhtml"}, htmldjango.GetLanguage, htmldjango.GetQuery},
	{"gotmpl", []string{".tpl", ".gotmpl", ".tmpl"}, gotmpl.GetLanguage, gotmpl.GetQuery},
	{"haml", []string{".haml"}, haml.GetLanguage, haml.GetQuery},
	{"slim", []string{".slim"}, slim.GetLanguage, slim.GetQuery},
	{"glimmer", []string{".gjs", ".gts", ".hbs"}, glimmer.GetLanguage, glimmer.GetQuery},
	{"razor", []string{".razor", ".cshtml"}, razor.GetLanguage, razor.GetQuery},
	{"templ", []string{".templ"}, templ.GetLanguage, templ.GetQuery},
	{"tera", []string{".tera"}, tera.GetLanguage, tera.GetQuery},
	{"mustache", []string{".mustache"}, mustache.GetLanguage, mustache.GetQuery},
	{"vento", []string{".vto"}, vento.GetLanguage, vento.GetQuery},
	{"superhtml", []string{".shtml"}, superhtml.GetLanguage, superhtml.GetQuery},
	{"heex", []string{".heex"}, heex.GetLanguage, heex.GetQuery},

	// Schemas / IaC / IDLs / config
	{"graphql", []string{".graphql", ".gql"}, graphql.GetLanguage, graphql.GetQuery},
	{"prisma", []string{".prisma"}, prisma.GetLanguage, prisma.GetQuery},
	{"jsonnet", []string{".jsonnet", ".libsonnet"}, jsonnet.GetLanguage, jsonnet.GetQuery},
	{"dhall", []string{".dhall"}, dhall.GetLanguage, dhall.GetQuery},
	{"cue", []string{".cue"}, cue.GetLanguage, cue.GetQuery},
	{"pkl", []string{".pkl"}, pkl.GetLanguage, pkl.GetQuery},
	{"nickel", []string{".ncl"}, nickel.GetLanguage, nickel.GetQuery},
	{"kcl", []string{".k"}, kcl.GetLanguage, kcl.GetQuery},
	{"bicep", []string{".bicep"}, bicep.GetLanguage, bicep.GetQuery},
	{"smithy", []string{".smithy"}, smithy.GetLanguage, smithy.GetQuery},
	{"capnp", []string{".capnp"}, capnp.GetLanguage, capnp.GetQuery},
	{"thrift", []string{".thrift"}, thrift.GetLanguage, thrift.GetQuery},
	{"kdl", []string{".kdl"}, kdl.GetLanguage, kdl.GetQuery},
	{"ron", []string{".ron"}, ron.GetLanguage, ron.GetQuery},
	{"typespec", []string{".tsp"}, typespec.GetLanguage, typespec.GetQuery},
	{"dbml", []string{".dbml"}, dbml.GetLanguage, dbml.GetQuery},
	{"hjson", []string{".hjson"}, hjson.GetLanguage, hjson.GetQuery},
	{"hocon", []string{".hocon"}, hocon.GetLanguage, hocon.GetQuery},
	{"ini", []string{".ini"}, ini.GetLanguage, ini.GetQuery},
	{"json5", []string{".json5"}, json5.GetLanguage, json5.GetQuery},
	{"jsonc", []string{".jsonc"}, jsonc.GetLanguage, jsonc.GetQuery},
	{"properties", []string{".properties"}, properties.GetLanguage, properties.GetQuery},
	{"scfg", []string{".scfg"}, scfg.GetLanguage, scfg.GetQuery},
	{"yang", []string{".yang"}, yang.GetLanguage, yang.GetQuery},
	{"xml", []string{".xml", ".xsd", ".xsl", ".xslt"}, xml.GetLanguage, xml.GetQuery},
	{"dtd", []string{".dtd"}, dtd.GetLanguage, dtd.GetQuery},
	{"editorconfig", []string{".editorconfig"}, editorconfig.GetLanguage, editorconfig.GetQuery},
	{"dotenv", []string{".env"}, dotenv.GetLanguage, dotenv.GetQuery},
	{"desktop", []string{".desktop"}, desktop.GetLanguage, desktop.GetQuery},
	{"devicetree", []string{".dts", ".dtsi"}, devicetree.GetLanguage, devicetree.GetQuery},
	{"kconfig", []string{".kconfig"}, kconfig.GetLanguage, kconfig.GetQuery},
	{"linkerscript", []string{".ld", ".lds"}, linkerscript.GetLanguage, linkerscript.GetQuery},

	// Shaders / hardware / low-level
	{"wgsl", []string{".wgsl"}, wgsl.GetLanguage, wgsl.GetQuery},
	{"glsl", []string{".glsl", ".vert", ".frag", ".geom", ".tesc", ".tese", ".comp"}, glsl.GetLanguage, glsl.GetQuery},
	{"hlsl", []string{".hlsl", ".hlsli"}, hlsl.GetLanguage, hlsl.GetQuery},
	{"cuda", []string{".cu", ".cuh"}, cuda.GetLanguage, cuda.GetQuery},
	{"ispc", []string{".ispc"}, ispc.GetLanguage, ispc.GetQuery},
	{"vhdl", []string{".vhd", ".vhdl"}, vhdl.GetLanguage, vhdl.GetQuery},
	{"systemverilog", []string{".sv", ".svh"}, systemverilog.GetLanguage, systemverilog.GetQuery},
	{"mlir", []string{".mlir"}, mlir.GetLanguage, mlir.GetQuery},
	{"llvm", []string{".ll"}, llvm.GetLanguage, llvm.GetQuery},
	{"jasmin", []string{".jasmin"}, jasmin.GetLanguage, jasmin.GetQuery},
	{"qbe", []string{".ssa"}, qbe.GetLanguage, qbe.GetQuery},
	{"firrtl", []string{".fir"}, firrtl.GetLanguage, firrtl.GetQuery},
	{"pioasm", []string{".pio"}, pioasm.GetLanguage, pioasm.GetQuery},
	{"gdshader", []string{".gdshader"}, gdshader.GetLanguage, gdshader.GetQuery},

	// Docs / typesetting
	{"latex", []string{".tex", ".ltx", ".sty", ".cls"}, latex.GetLanguage, latex.GetQuery},
	{"typst", []string{".typ"}, typst.GetLanguage, typst.GetQuery},
	{"asciidoc", []string{".adoc", ".asciidoc"}, asciidoc.GetLanguage, asciidoc.GetQuery},
	{"djot", []string{".dj"}, djot.GetLanguage, djot.GetQuery},
	{"mermaid", []string{".mmd", ".mermaid"}, mermaid.GetLanguage, mermaid.GetQuery},
	{"norg", []string{".norg"}, norg.GetLanguage, norg.GetQuery},
	{"bibtex", []string{".bib"}, bibtex.GetLanguage, bibtex.GetQuery},
	{"plantuml", []string{".puml", ".plantuml", ".pu"}, plantuml.GetLanguage, plantuml.GetQuery},

	// Functional / niche general-purpose
	{"agda", []string{".agda", ".lagda"}, agda.GetLanguage, agda.GetQuery},
	{"idris", []string{".idr", ".lidr"}, idris.GetLanguage, idris.GetQuery},
	{"purescript", []string{".purs"}, purescript.GetLanguage, purescript.GetQuery},
	{"roc", []string{".roc"}, roc.GetLanguage, roc.GetQuery},
	{"gren", []string{".gren"}, gren.GetLanguage, gren.GetQuery},
	{"fennel", []string{".fnl"}, fennel.GetLanguage, fennel.GetQuery},
	{"janet", []string{".janet"}, janet.GetLanguage, janet.GetQuery},
	{"hack", []string{".hack"}, hack.GetLanguage, hack.GetQuery},
	{"haxe", []string{".hx"}, haxe.GetLanguage, haxe.GetQuery},
	{"pony", []string{".pony"}, pony.GetLanguage, pony.GetQuery},
	{"c3", []string{".c3"}, c3.GetLanguage, c3.GetQuery},
	{"aiken", []string{".ak"}, aiken.GetLanguage, aiken.GetQuery},
	{"effekt", []string{".effekt"}, effekt.GetLanguage, effekt.GetQuery},
	{"eiffel", []string{".e"}, eiffel.GetLanguage, eiffel.GetQuery},
	{"jule", []string{".jule"}, jule.GetLanguage, jule.GetQuery},
	{"koka", []string{".kk"}, koka.GetLanguage, koka.GetQuery},
	{"luau", []string{".luau"}, luau.GetLanguage, luau.GetQuery},
	{"moonbit", []string{".mbt"}, moonbit.GetLanguage, moonbit.GetQuery},
	{"motoko", []string{".mo"}, motoko.GetLanguage, motoko.GetQuery},
	{"ralph", []string{".ral"}, ralph.GetLanguage, ralph.GetQuery},
	{"scheme", []string{".scm", ".ss"}, scheme.GetLanguage, scheme.GetQuery},
	{"sml", []string{".sml", ".sig"}, sml.GetLanguage, sml.GetQuery},
	// unison: skipped — see import block.
	{"wing", []string{".w"}, wing.GetLanguage, wing.GetQuery},
	{"commonlisp", []string{".lisp", ".cl"}, commonlisp.GetLanguage, commonlisp.GetQuery},

	// Build / DSL / accounting / testing
	{"meson", []string{".meson"}, meson.GetLanguage, meson.GetQuery},
	{"just", []string{".just"}, just.GetLanguage, just.GetQuery},
	{"beancount", []string{".beancount", ".bean"}, beancount.GetLanguage, beancount.GetQuery},
	{"ledger", []string{".ledger"}, ledger.GetLanguage, ledger.GetQuery},
	{"gherkin", []string{".feature"}, gherkin.GetLanguage, gherkin.GetQuery},
	{"hurl", []string{".hurl"}, hurl.GetLanguage, hurl.GetQuery},
	{"robot", []string{".robot", ".resource"}, robot.GetLanguage, robot.GetQuery},
	{"earthfile", []string{".earthfile"}, earthfile.GetLanguage, earthfile.GetQuery},
	{"ninja", []string{".ninja"}, ninja.GetLanguage, ninja.GetQuery},
	{"bitbake", []string{".bb", ".bbappend", ".bbclass"}, bitbake.GetLanguage, bitbake.GetQuery},
	{"caddy", []string{".caddyfile"}, caddy.GetLanguage, caddy.GetQuery},
	{"snakemake", []string{".smk"}, snakemake.GetLanguage, snakemake.GetQuery},
	{"gn", []string{".gn", ".gni"}, gn.GetLanguage, gn.GetQuery},
	{"cooklang", []string{".cook"}, cooklang.GetLanguage, cooklang.GetQuery},
	{"requirements", []string{".reqs"}, requirements.GetLanguage, requirements.GetQuery},

	// DSL / auth / logic / formal
	{"cedar", []string{".cedar"}, cedar.GetLanguage, cedar.GetQuery},
	{"cel", []string{".cel"}, cel.GetLanguage, cel.GetQuery},
	{"circom", []string{".circom"}, circom.GetLanguage, circom.GetQuery},
	{"clarity", []string{".clar"}, clarity.GetLanguage, clarity.GetQuery},
	{"rego", []string{".rego"}, rego.GetLanguage, rego.GetQuery},
	{"tlaplus", []string{".tla"}, tlaplus.GetLanguage, tlaplus.GetQuery},
	{"quint", []string{".qnt"}, quint.GetLanguage, quint.GetQuery},
	{"structurizr", []string{".structurizr"}, structurizr.GetLanguage, structurizr.GetQuery},
	{"gritql", []string{".grit"}, gritql.GetLanguage, gritql.GetQuery},
	{"ql", []string{".ql"}, ql.GetLanguage, ql.GetQuery},

	// Database / query
	{"sparql", []string{".sparql", ".rq"}, sparql.GetLanguage, sparql.GetQuery},
	{"surrealql", []string{".surql"}, surrealql.GetLanguage, surrealql.GetQuery},
	{"promql", []string{".promql"}, promql.GetLanguage, promql.GetQuery},
	{"kusto", []string{".kql"}, kusto.GetLanguage, kusto.GetQuery},
	{"soql", []string{".soql"}, soql.GetLanguage, soql.GetQuery},
	{"sosl", []string{".sosl"}, sosl.GetLanguage, sosl.GetQuery},
	{"prql", []string{".prql"}, prql.GetLanguage, prql.GetQuery},
	{"turtle", []string{".ttl"}, turtle.GetLanguage, turtle.GetQuery},

	// Data formats
	{"tsv", []string{".tsv"}, tsv.GetLanguage, tsv.GetQuery},
	{"psv", []string{".psv"}, psv.GetLanguage, psv.GetQuery},
	{"textproto", []string{".textproto", ".pbtxt"}, textproto.GetLanguage, textproto.GetQuery},
	{"po", []string{".po", ".pot"}, po.GetLanguage, po.GetQuery},
	{"pgn", []string{".pgn"}, pgn.GetLanguage, pgn.GetQuery},
	{"todotxt", []string{".todotxt"}, todotxt.GetLanguage, todotxt.GetQuery},

	// Frontend stack / module manifests
	{"gomod", []string{".gomod"}, gomod.GetLanguage, gomod.GetQuery},
	{"gosum", []string{".gosum"}, gosum.GetLanguage, gosum.GetQuery},
	{"gowork", []string{".gowork"}, gowork.GetLanguage, gowork.GetQuery},
	{"godot_resource", []string{".tres", ".tscn", ".godot"}, godot_resource.GetLanguage, godot_resource.GetQuery},

	// Shells / scripts
	{"fish", []string{".fish"}, fish.GetLanguage, fish.GetQuery},
	{"nushell", []string{".nu"}, nu.GetLanguage, nu.GetQuery},
	{"jq", []string{".jq"}, jq.GetLanguage, jq.GetQuery},
	{"awk", []string{".awk"}, awk.GetLanguage, awk.GetQuery},
	{"elvish", []string{".elv"}, elvish.GetLanguage, elvish.GetQuery},

	// Config files (dotfile / system)
	{"git_config", []string{".gitconfig"}, git_config.GetLanguage, git_config.GetQuery},
	{"gitattributes", []string{".gitattributes"}, gitattributes.GetLanguage, gitattributes.GetQuery},
	{"gitcommit", []string{".gitcommit"}, gitcommit.GetLanguage, gitcommit.GetQuery},
	{"gitignore", []string{".gitignore"}, gitignore.GetLanguage, gitignore.GetQuery},
	{"hyprlang", []string{".hyprlang"}, hyprlang.GetLanguage, hyprlang.GetQuery},
	{"nftables", []string{".nft"}, nftables.GetLanguage, nftables.GetQuery},
	{"passwd", []string{".passwd"}, passwd.GetLanguage, passwd.GetQuery},
	{"pem", []string{".pem", ".crt"}, pem.GetLanguage, pem.GetQuery},
	{"poe_filter", []string{".poefilter"}, poe_filter.GetLanguage, poe_filter.GetQuery},
	{"puppet", []string{".pp"}, puppet.GetLanguage, puppet.GetQuery},
	{"ssh_config", []string{".sshconfig"}, ssh_config.GetLanguage, ssh_config.GetQuery},
	{"sxhkdrc", []string{".sxhkdrc"}, sxhkdrc.GetLanguage, sxhkdrc.GetQuery},
	{"tmux", []string{".tmuxconf"}, tmux.GetLanguage, tmux.GetQuery},

	// Misc useful
	{"dot", []string{".dot", ".gv"}, dot.GetLanguage, dot.GetQuery},
	{"gnuplot", []string{".gp", ".gnu", ".gnuplot", ".plt"}, gnuplot.GetLanguage, gnuplot.GetQuery},
	{"gpg", []string{".gpgconf"}, gpg.GetLanguage, gpg.GetQuery},
	{"strace", []string{".strace"}, strace.GetLanguage, strace.GetQuery},
	{"vrl", []string{".vrl"}, vrl.GetLanguage, vrl.GetQuery},
	{"zeek", []string{".zeek"}, zeek.GetLanguage, zeek.GetQuery},
	{"ziggy", []string{".ziggy"}, ziggy.GetLanguage, ziggy.GetQuery},
	{"ziggy_schema", []string{".ziggy-schema"}, ziggy_schema.GetLanguage, ziggy_schema.GetQuery},
	{"starlark", []string{".star", ".bzl"}, starlark.GetLanguage, starlark.GetQuery},
	{"sourcepawn", []string{".sp"}, sourcepawn.GetLanguage, sourcepawn.GetQuery},
	{"scss", []string{".scss"}, scss.GetLanguage, scss.GetQuery},
	{"rbs", []string{".rbs"}, rbs.GetLanguage, rbs.GetQuery},
	{"ocamllex", []string{".mll"}, ocamllex.GetLanguage, ocamllex.GetQuery},
	{"dataweave", []string{".dwl"}, dataweave.GetLanguage, dataweave.GetQuery},
	{"usd", []string{".usd", ".usda"}, usd.GetLanguage, usd.GetQuery},
	{"wit", []string{".wit"}, wit.GetLanguage, wit.GetQuery},
}

// registerForestLanguages adds every forest-backed signature-only
// extractor to the registry, skipping any language whose name OR
// any declared extension is already claimed by a hand-written
// extractor. Bespoke depth always wins.
func registerForestLanguages(reg *parser.Registry) {
	for _, fl := range forestLanguages {
		if _, exists := reg.GetByLanguage(fl.name); exists {
			continue
		}
		conflict := false
		for _, ext := range fl.exts {
			if _, exists := reg.GetByExtension(ext); exists {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		reg.Register(forest.New(fl.name, fl.exts, fl.getLang, fl.getQry))
	}
}
