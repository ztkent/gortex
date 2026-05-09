# Supported Languages

Gortex currently indexes **92 languages**. Each language has an extractor that
walks the source, emits symbols (functions, methods, types, interfaces,
variables) into the graph, and records `imports` / `calls` edges.

Two engine tiers are used:

- **tree-sitter** — full concrete syntax tree via a vendored grammar. Produces
  high-fidelity symbols, precise call edges, and accurate node ranges.
- **regex** — pattern-matched line scanning with indent / brace / keyword block
  heuristics. Captures top-level symbols and imports; call edges vary per
  language.

For sixteen of these languages an LSP server can additionally upgrade
edges from `ast_inferred` to `lsp_resolved` and unlock the
on-demand action tools (`get_diagnostics`, `get_code_actions`,
`apply_code_action`, `fix_all_in_file`). See **[lsp.md](lsp.md)** for the
server matrix, install commands, lifecycle knobs, and config schema.

## At a glance

| Category | Count | Languages |
|---|---|---|
| Core programming | 10 | Go, TypeScript, JavaScript, Python, Rust, Java, C#, C, C++, Kotlin |
| JVM, .NET, systems | 10 | Scala, Swift, PHP, Ruby, Groovy, F#, D, Zig, Vala, Objective-C |
| Scripting & shell | 10 | Bash, PowerShell, Batch, Perl, Raku, Lua, Tcl, VimScript, AutoHotkey, CoffeeScript |
| Functional | 8 | Haskell, OCaml, Elixir, Clojure, Erlang, Racket, Gleam, Emacs Lisp |
| Systems / emerging | 8 | Nim, Crystal, Mojo, Odin, V, Hare, Carbon, ReScript |
| Scientific & enterprise | 12 | Julia, R, MATLAB, Mathematica, SAS, Stata, Fortran, COBOL, Ada, Pascal, ABAP, Apex |
| Mobile & game | 4 | Dart, GDScript, Verse, ActionScript |
| Blockchain / smart contracts | 6 | Solidity, Move, Cairo, Noir, Tact, Ballerina |
| Template engines | 8 | Blade, EJS, Handlebars, Jinja, Twig, ERB, Liquid, Pug |
| Data, config, build | 12 | JSON, YAML, TOML, HCL/Terraform, SQL, Protobuf, Markdown, HTML, CSS, Dockerfile, Makefile, CMake |
| Niche / domain | 4 | Nix, AL (Business Central), Assembly (NASM/GAS/ARM/WLA-DX/CA65), Shaders (GLSL/HLSL) |
| **Total** | **92** | |

## Core programming — deep extraction

Tree-sitter-backed languages with the most thorough extraction. `Meta["methods"]`
on interface nodes stores the expected method set for implementation matching.

| Language | Functions | Methods + MemberOf | Types | Interfaces | Imports | Calls | Variables |
|----------|-----------|-------------------|-------|------------|---------|-------|-----------|
| Go | Full | Full (receiver) | Full | Full + Meta["methods"] | Full | Full | Full |
| TypeScript | Full | Full | Full | Full + Meta["methods"] | Full | Full | Full |
| JavaScript | Full | Full | Full | - | Full | Full | Full |
| Python | Full | Full | Full | - | Full | Full | Partial |
| Rust | Full | Full (impl blocks) | Full | Full + Meta["methods"] | Full | Full | Full |
| Java | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| C# | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| Kotlin | Full | Full | Full | Full | Full | Full | Properties |
| Scala | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| Swift | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| PHP | Full | Full | Full | Full | Full | Full | - |
| Ruby | Full | Full | Full | - | Full | Full | Constants |
| Elixir | Full | Full (defmodule) | Modules | - | Full | Full | Attributes |
| C | Full | - | Structs/Enums | - | Full | Full | Globals |
| C++ | Full | Full | Classes/Structs | - | Full | Full | - |
| Dart | Full | Full | Classes/Enums/Mixins/Extensions | Abstract interface | Full | Full | Full |
| OCaml | Full | Full (class) | Types/Modules | Module types | open | Full | Full |
| Lua | Full | Full (M.func/M:method) | - | - | require() | Full | Full |

## Data, config, build

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| JSON | `.json`, `.json5`, `.jsonc` | Top-level keys |
| YAML | `.yaml`, `.yml` | Top-level keys |
| TOML | `.toml` | Tables, key-value pairs |
| HCL / Terraform | `.tf`, `.tfvars`, `.hcl` | Resource / data / module / variable / output blocks |
| SQL | `.sql` | Tables (with columns), views, functions, indexes, triggers |
| Protobuf | `.proto` | Messages (with fields), services + RPCs, enums, imports |
| Markdown | `.md` | Headings, local file links, code-block languages |
| HTML | `.html`, `.htm` | Script / link references, element IDs |
| CSS | `.css` | Class selectors, ID selectors, custom properties, `@import` |
| Dockerfile | `Dockerfile`, `Containerfile`, `.dockerfile` | `FROM` (base images), `ENV` / `ARG` variables |
| Makefile | `Makefile`, `GNUmakefile`, `.mk`, `.make` | Targets, `define…endef`, `VAR = …`, `include` / `-include` |
| CMake | `CMakeLists.txt`, `.cmake` | `function(…)`, `macro(…)`, `add_library`, `add_executable`, `include(…)`, `set(…)` |

## Template engines

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Blade (Laravel) | `.blade`, `.blade.php` | `@section` / `@yield` / `@component` / `@include`; `@extends` → import |
| EJS | `.ejs` | JS `function` / arrow inside `<% … %>`; `include('x')` → import |
| Handlebars / Mustache | `.hbs`, `.handlebars`, `.mustache` | `{{#block}}` as function; `{{> partial}}` → import; helper calls as edges |
| Jinja | `.jinja`, `.jinja2`, `.j2` | `{% block %}` / `{% macro %}`; `extends` / `include` / `import` / `from … import` |
| Twig | `.twig` | Same shape as Jinja |
| ERB | `.erb`, `.rhtml`, `.html.erb`, `.js.erb`, `.css.erb`, `.json.erb` | Ruby `def` / `class` inside `<% … %>`; `render 'x'` → import |
| Liquid | `.liquid` | `{% capture %}` as function; `{% assign %}` as variable; `{% include/render %}` → import |
| Pug | `.pug`, `.jade` | `mixin` / `block NAME` as function; `extends` / `include` → import |

## Blockchain / smart contracts

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Solidity | `.sol` | Contracts, functions, events, modifiers, structs |
| Move (Sui/Aptos) | `.move` | `module`, `fun` / `public fun` / `entry fun`, `struct`, `use X::Y` |
| Cairo (StarkNet) | `.cairo` | `fn`, `struct` / `enum` / `trait` / `mod`, `use X::Y` |
| Noir (Aztec) | `.nr` | `fn`, `struct` / `trait` / `impl` / `mod`, `use dep::X::Y` |
| Tact (TON) | `.tact` | `contract` / `trait` / `message` / `struct`, `fun` / `receive` / `init`, `import "X"` |
| Ballerina | `.bal` | `function`, `service NAME on …`, `type NAME record {…}`, `class`, `import X/Y` |

## Scientific & enterprise

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Julia | `.jl` | `function`, `struct`, `module`, `using` / `import` |
| R | `.r`, `.R` | Function defs; `library` / `require` / `source` |
| MATLAB | `.mlx` | `function` (end-terminated), `classdef`, `import a.b.c` |
| Mathematica | `.wl`, `.wls`, `.nb` | `name[args_] := body`, `SetDelayed`, `Get[…]` / `Needs[…]` |
| SAS | `.sas` | `proc` / `%macro` as function, `data` as variable, `%include` / `libname` |
| Stata | `.do`, `.ado` | `program define`, `local` / `global`, `use` / `do` / `include` |
| Fortran | `.f`, `.f90`, `.f95`, `.f03`, `.f08` | `subroutine` / `function` / `module`, `use X` |
| COBOL | `.cob`, `.cbl`, `.cpy` | Programs, paragraphs, sections, `COPY` |
| Ada | `.ada`, `.adb`, `.ads` | Packages, procedures, functions, `with` |
| Pascal / Delphi | `.pas`, `.pp`, `.dpr` | Units, procedures, functions, classes |
| ABAP (SAP) | `.abap` | `FORM` / `FUNCTION` / `METHOD` / `CLASS…DEFINITION`, `INCLUDE` |
| Apex (Salesforce) | `.cls`, `.trigger`, `.apex` | Classes, triggers, methods |

## Emerging languages

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Mojo | `.mojo`, `.🔥` | `fn` / `def`, `struct` / `trait`, `from … import` / `import` |
| Odin | `.odin` | `name :: proc`, `name :: struct` / `enum` / `union`, `import "X"` / `foreign import` |
| V | `.v`, `.vsh` | `fn`, `struct` / `interface` / `enum` / `type`, `import`, `module` |
| Hare | `.ha` | `[export] fn`, `type X = struct` / `union` / `enum`, `use X;` |
| Carbon | `.carbon` | `fn`, `class` / `interface` / `adapter` / `choice`, `import` |
| ReScript | `.res`, `.resi` | `let` (function / variable), `type`, `module`, `open` / `include` |
| Gleam | `.gleam` | `[pub] fn`, `[pub] type`, `import X/Y` / `import X.{Y}` |

## Scripting & shell

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Bash / Zsh | `.sh`, `.bash`, `.zsh` | Function defs, `source` / `.` |
| PowerShell | `.ps1`, `.psm1`, `.psd1` | `function`, `class`, `using` |
| Batch | `.bat`, `.cmd` | `:LABEL` as function, `call :LABEL` / `goto` as call edges |
| Perl | `.pl`, `.pm`, `.t` | `sub`, `package`, `use` / `require` |
| Raku | `.raku`, `.rakumod`, `.p6` | `sub`, `class`, `use` |
| Lua | `.lua` | Full tree-sitter (see core matrix) |
| Tcl | `.tcl` | `proc`, `package require`, `source` |
| VimScript | `.vim`, `.vimrc` | `function`, `command`, `source` |
| AutoHotkey | `.ahk`, `.ahk1`, `.ahk2` | Hotkeys, labels, functions (v1 + v2) |
| CoffeeScript | `.coffee` | `name = (args) ->` / `=>`, `class`, `require 'X'` |

## Functional

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| Haskell | `.hs`, `.lhs` | Full (see core matrix) |
| OCaml | `.ml`, `.mli` | Full (see core matrix) |
| Clojure | `.clj`, `.cljs`, `.cljc`, `.edn` | `defn`, `defrecord` / `deftype`, `defprotocol`, `require` / `use` |
| Erlang | `.erl`, `.hrl` | Functions, `-type` / `-record`, `-import` |
| Elixir | `.ex`, `.exs` | Full (see core matrix) |
| Racket | `.rkt`, `.ss` | `define`, `struct`, `require` |
| F# | `.fs`, `.fsi`, `.fsx` | `let`, `type`, `module`, `open` |
| Emacs Lisp | `.el` | `defun`, `defvar`, `defmacro`, `require` |

## Systems, mobile, game, niche

| Language | Extensions | What it extracts |
|----------|------------|------------------|
| D | `.d`, `.di` | `struct` / `class` / `interface` / `enum` / `union` / `template`, `import X.Y` |
| Zig | `.zig`, `.zon` | Structs / enums / unions, `@import`, functions, globals |
| Nim | `.nim`, `.nims`, `.nimble` | `proc` / `func` / `method` / `iterator` / `template` / `macro`, type defs, `import` |
| Crystal | `.cr` | `def`, `class`, `module`, `require` |
| Vala | `.vala`, `.vapi` | `namespace` / `class` / `interface` / `struct` / `enum`, methods, `using X;` |
| Groovy / Gradle | `.groovy`, `.gvy`, `.gy`, `.gradle` | Classes, `def`, imports |
| Objective-C(++) | `.m`, `.mm` | `@interface` / `@protocol` / `@implementation`, method selectors, `#import` / `@import` |
| ActionScript | `.as` | `package`, classes, interfaces, `function`, `import X.Y.*;` |
| Dart | `.dart` | Full (see core matrix) |
| Swift | `.swift` | Full (see core matrix) |
| GDScript | `.gd` | `func`, `class`, signals |
| Verse (UEFN) | `.verse` | `class` / `struct` / `enum` / `interface`, functions with specifier blocks, `using { /Path }` |
| Nix | `.nix` | Attribute sets, functions, `import` / `<nixpkgs>` |
| AL (Business Central) | `.al` | Tables, pages, codeunits, procedures |
| Assembly | `.asm`, `.s`, `.S`, `.nasm`, `.masm`, `.inc`, `.a65` | Labels as functions; `call` / `jsr` / `bl` / `jmp` as edges; NASM/MASM/GAS/WLA-DX/CA65/ARM |
| Shaders | `.glsl`, `.vert`, `.frag`, `.hlsl`, `.compute` | Functions, uniforms, `#include` |

## Extension collisions

A few extensions conflict across languages; the registration order in
`internal/parser/languages/register.go` decides which extractor wins.

| Extension | Registered as | Alternative |
|-----------|---------------|-------------|
| `.m` | Objective-C | MATLAB (uses `.mlx` instead) |
| `.v` | V | Verilog / Coq (not yet supported) |
| `.d` | D | D import files (`.di`) |
| `.as` | ActionScript | AssemblyScript (not supported) |

## Adding a language

New extractors go in `internal/parser/languages/`. Follow the template of
[`nim.go`](../internal/parser/languages/nim.go) (regex-based) or
[`golang.go`](../internal/parser/languages/golang.go) (tree-sitter). Register
in [`register.go`](../internal/parser/languages/register.go) and add a
`_test.go` with at least a happy-path and empty-input case. Shared helpers
live in [`helpers_indent.go`](../internal/parser/languages/helpers_indent.go)
(`findBlockEnd`, `findIndentedBlockEnd`, `findKeywordBlockEnd`, `lineAt`).
