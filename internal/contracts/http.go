package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// HTTPExtractor detects HTTP route provider and consumer patterns across
// multiple languages using regex matching on source text.
type HTTPExtractor struct{}

var _ Extractor = (*HTTPExtractor)(nil)

// SupportedLanguages returns the languages this extractor can analyse.
func (h *HTTPExtractor) SupportedLanguages() []string {
	return []string{
		"go", "typescript", "javascript", "python",
		"java", "kotlin", "dart",
		"rust", "csharp", "ruby", "php", "elixir",
	}
}

// httpPattern describes a single regex pattern that matches an HTTP route
// declaration or call.
type httpPattern struct {
	re        *regexp.Regexp
	role      Role
	method    string // HTTP method (empty = extract from match)
	methodGrp int    // capture group index for method when not fixed
	pathGrp   int    // capture group index for path
	// handlerGrp is the capture group for the handler identifier on the
	// provider side (e.g. `listUsers` in `r.GET("/users", listUsers)`).
	// 0 = not captured. When set and the capture resolves to a function
	// node in the same file, the Contract's SymbolID is the handler, not
	// the enclosing registration function — so "trace a request" queries
	// land on the business logic instead of setupRoutes().
	handlerGrp int
	framework  string
	confidence float64
	languages  []string // empty = all
}

// Compiled patterns -----------------------------------------------------------

var httpPatterns = []httpPattern{
	// ---- Go providers (high confidence, framework-specific) ----
	// Go 1.22+ stdlib mux: mux.HandleFunc("METHOD /path", h). The
	// method is embedded in the pattern as a prefix and must be
	// split out so the resulting contract ID matches the consumer
	// side's http::METHOD::path shape.
	{
		re:         regexp.MustCompile(`(?:Handle|HandleFunc)\(\s*["` + "`" + `](GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(/[^"` + "`" + `]*)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "net/http",
		confidence: 0.95,
		languages:  []string{"go"},
	},
	// Legacy net/http HandleFunc with pattern-only path. Requires the
	// captured path to start with "/" (no leading verb), so the Go
	// 1.22+ "METHOD /path" form above doesn't double-match and emit
	// a bogus http::ANY::/VERB path contract alongside the canonical
	// http::VERB::/path one.
	{
		re:         regexp.MustCompile(`(?:Handle|HandleFunc)\(\s*["` + "`" + `](/[^"` + "`" + `]*)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		handlerGrp: 2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		// Match router/group method calls but not http.Get/http.Post (stdlib consumers).
		re:         regexp.MustCompile(`(?:^|[^/])\b(?:r|g|e|router|group|api|v1|mux|app)\.(Get|Post|Put|Delete|Patch|Head|Options)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "gin/echo/chi",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		re:         regexp.MustCompile(`\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "fiber",
		confidence: 0.9,
		languages:  []string{"go"},
	},

	// ---- TS/JS providers ----
	{
		re:         regexp.MustCompile(`(?:app|router)\.(get|post|put|delete|patch|head|options|all)\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*(?:,\s*(\w+))?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "express",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},
	{
		re:         regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Head|Options)\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "nestjs",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},

	// ---- Python providers ----
	{
		re:         regexp.MustCompile(`@\w+\.(get|post|put|delete|patch|head|options)\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "fastapi/flask",
		confidence: 0.9,
		languages:  []string{"python"},
	},
	{
		re:         regexp.MustCompile(`@\w+\.route\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		framework:  "flask",
		confidence: 0.9,
		languages:  []string{"python"},
	},
	{
		re:         regexp.MustCompile(`path\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		framework:  "django",
		confidence: 0.7,
		languages:  []string{"python"},
	},

	// ---- Java providers ----
	{
		re:         regexp.MustCompile(`@(Get|Post|Put|Delete|Patch)Mapping\(\s*(?:value\s*=\s*)?["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "spring",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},
	{
		re:         regexp.MustCompile(`@RequestMapping\(\s*(?:value\s*=\s*)?["']([^"']+)["']`),
		role:       RoleProvider,
		method:     "ANY",
		pathGrp:    1,
		framework:  "spring",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},
	{
		re:         regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH)\s+@Path\(\s*["']([^"']+)["']`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "jaxrs",
		confidence: 0.9,
		languages:  []string{"java", "kotlin"},
	},

	// ---- Go consumers ----
	{
		re:         regexp.MustCompile(`http\.(Get|Post|Head)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},
	{
		re:         regexp.MustCompile(`http\.NewRequest\(\s*["` + "`" + `](\w+)["` + "`" + `]\s*,\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "net/http",
		confidence: 0.9,
		languages:  []string{"go"},
	},

	// ---- TS/JS consumers ----
	// fetch with explicit `method: '<VERB>'` in the options object —
	// tried first so the generic GET pattern below doesn't steal the
	// match.
	{
		re:         regexp.MustCompile(`fetch\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `][^)]*?method\s*:\s*["'](\w+)["']`),
		role:       RoleConsumer,
		methodGrp:  2,
		pathGrp:    1,
		framework:  "fetch",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},
	{
		re:         regexp.MustCompile(`fetch\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleConsumer,
		method:     "GET",
		pathGrp:    1,
		framework:  "fetch",
		confidence: 0.7,
		languages:  []string{"typescript", "javascript"},
	},
	// Axios. The optional `<...>` between the method name and the
	// opening paren is TypeScript's generic form — axios.post<Resp, Req>(...)
	// — which the enrichment layer uses to pin response / request
	// types. `[^<>(),]` inside the generic keeps the matcher fast and
	// stops greedy consumption from crossing the path argument.
	{
		re:         regexp.MustCompile(`axios\.(get|post|put|delete|patch|head|options)(?:<[^<>()]*>)?\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "axios",
		confidence: 0.9,
		languages:  []string{"typescript", "javascript"},
	},

	// ---- Python consumers ----
	{
		re:         regexp.MustCompile(`(?:requests|httpx)\.(get|post|put|delete|patch|head|options)\(\s*["']([^"']+)["']`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "requests/httpx",
		confidence: 0.9,
		languages:  []string{"python"},
	},

	// ---- Java consumers (generic) ----
	{
		re:         regexp.MustCompile(`(?:HttpClient|RestTemplate|WebClient).*["']([^"']+)["']`),
		role:       RoleConsumer,
		method:     "GET",
		pathGrp:    1,
		framework:  "java-http",
		confidence: 0.7,
		languages:  []string{"java", "kotlin"},
	},

	// ---- Dart consumers ----
	// Dio (the dominant HTTP client in modern Flutter apps). Matches
	// identifiers like `dio`, `_dio`, `apiDio` etc. invoking a method
	// with a string-literal path.
	{
		re:         regexp.MustCompile(`\b_?\w*[Dd]io\.(get|post|put|delete|patch|head)\(\s*['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "dio",
		confidence: 0.9,
		languages:  []string{"dart"},
	},
	// package:http functional API — http.get(Uri.parse('/x')) or
	// http.post('/x'). The regex captures either the string inside
	// Uri.parse or the direct literal argument.
	{
		re:         regexp.MustCompile(`\bhttp\.(get|post|put|delete|patch|head)\(\s*(?:Uri\.parse\(\s*)?['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "package:http",
		confidence: 0.8,
		languages:  []string{"dart"},
	},

	// ---- Rust providers ----
	// Axum: `Router::new().route("/users", get(handler))` and
	// `Router::new().route("/users/:id", post(create).delete(remove))`.
	// The method comes from the `get|post|...` function call; the
	// path is the first string literal in `.route(`.
	{
		re:         regexp.MustCompile(`\.route\(\s*"([^"]+)"\s*,\s*(get|post|put|delete|patch|head|options)\(\s*(\w+)?`),
		role:       RoleProvider,
		methodGrp:  2,
		pathGrp:    1,
		handlerGrp: 3,
		framework:  "axum",
		confidence: 0.9,
		languages:  []string{"rust"},
	},
	// Actix-web macro form: `#[get("/path")]` / `#[post("/path")]`.
	{
		re:         regexp.MustCompile(`#\[(get|post|put|delete|patch|head|options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "actix",
		confidence: 0.95,
		languages:  []string{"rust"},
	},
	// Rocket macro form: `#[get("/path")]` (same syntax as Actix —
	// the detection code can't tell them apart from the route line
	// alone; tag as "rust" and let repo context disambiguate).
	// Covered by the Actix regex above.

	// ---- Rust consumers ----
	// reqwest: `client.get("/users")`, `Client::new().post("/users")`.
	{
		re:         regexp.MustCompile(`\b\w+\.(get|post|put|delete|patch|head)\(\s*(?:&?format!\(\s*)?"([^"]+)"`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "reqwest",
		confidence: 0.7,
		languages:  []string{"rust"},
	},

	// ---- C# ASP.NET providers ----
	// Attribute routing: `[HttpGet("/path")]`, `[HttpPost]` +
	// `[Route("path")]`. First form is the clean one.
	{
		re:         regexp.MustCompile(`\[Http(Get|Post|Put|Delete|Patch|Head|Options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "aspnet",
		confidence: 0.95,
		languages:  []string{"csharp"},
	},
	// Minimal APIs: `app.MapGet("/path", handler)`.
	{
		re:         regexp.MustCompile(`\b(?:app|routes?)\.Map(Get|Post|Put|Delete|Patch|Head|Options)\(\s*"([^"]+)"`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "aspnet",
		confidence: 0.9,
		languages:  []string{"csharp"},
	},

	// ---- C# consumers ----
	// HttpClient: `client.GetAsync("/path")`, `PostAsync`, etc.
	{
		re:         regexp.MustCompile(`\b\w+\.(Get|Post|Put|Delete|Patch|Head|Options)(?:Async|String)?\(\s*"([^"]+)"`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "httpclient",
		confidence: 0.7,
		languages:  []string{"csharp"},
	},

	// ---- Ruby on Rails providers ----
	// Explicit route: `get '/users', to: 'users#index'`,
	// `post '/users' => 'users#create'`.
	{
		re:         regexp.MustCompile(`(?m)^\s*(get|post|put|patch|delete|head|options)\s+['"]([^'"]+)['"]\s*(?:,|=>)`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "rails",
		confidence: 0.9,
		languages:  []string{"ruby"},
	},

	// ---- Ruby consumers ----
	// Net::HTTP.get(URI("http://..."))  — low confidence, skip.
	// Faraday: `conn.post('/users')` — generic consumer.
	{
		re:         regexp.MustCompile(`\b\w+\.(get|post|put|delete|patch|head)\(\s*['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "faraday",
		confidence: 0.6,
		languages:  []string{"ruby"},
	},

	// ---- PHP Laravel providers ----
	// `Route::get('/path', ...)`, `Route::post('/path', [Controller::class, 'method'])`.
	{
		re:         regexp.MustCompile(`Route::(get|post|put|patch|delete|head|options)\(\s*['"]([^'"]+)['"]`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "laravel",
		confidence: 0.95,
		languages:  []string{"php"},
	},
	// Symfony attribute routing: `#[Route("/path", methods: ["POST"])]`.
	{
		re:         regexp.MustCompile(`#\[Route\(\s*['"]([^'"]+)['"][^)]*methods:\s*\[\s*['"](\w+)['"]`),
		role:       RoleProvider,
		methodGrp:  2,
		pathGrp:    1,
		framework:  "symfony",
		confidence: 0.9,
		languages:  []string{"php"},
	},

	// ---- PHP consumers ----
	// Guzzle: `$client->get('/path')`, `$client->request('POST', '/path')`.
	{
		re:         regexp.MustCompile(`\$\w+->(get|post|put|delete|patch|head)\(\s*['"]([^'"]+)['"]`),
		role:       RoleConsumer,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "guzzle",
		confidence: 0.8,
		languages:  []string{"php"},
	},

	// ---- Elixir Phoenix providers ----
	// `get "/users", UserController, :index` inside router.ex scope.
	{
		re:         regexp.MustCompile(`(?m)^\s*(get|post|put|patch|delete|head|options)\s+"([^"]+)"\s*,`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		framework:  "phoenix",
		confidence: 0.9,
		languages:  []string{"elixir"},
	},

	// ---- Dart providers (shelf_router) ----
	// `router.get('/path', handler)` and Dart's cascade form
	// `..get('/path', handler)` — the latter dominates in
	// idiomatic shelf_router code.
	{
		re:         regexp.MustCompile(`(?:\.\.|\b\w*[Rr]outer\.)(get|post|put|delete|patch|head|options)\(\s*['"]([^'"]+)['"]\s*,\s*(\w+)?`),
		role:       RoleProvider,
		methodGrp:  1,
		pathGrp:    2,
		handlerGrp: 3,
		framework:  "shelf_router",
		confidence: 0.85,
		languages:  []string{"dart"},
	},
}

// httpPrefilterMarkers is the per-language substring prefilter.
// Files whose language appears here must contain at least one
// marker before the ~30 HTTP regexes run — otherwise we skip the
// file entirely. See the gRPC reference implementation for the
// pattern's origin. Languages whose HTTP patterns hinge on
// bare keywords (python's `path(`, ruby/elixir's top-level `get`,
// `post`) are intentionally absent: any marker tight enough to
// reject non-HTTP files would also reject legitimate HTTP files,
// so the regex scan carries the whole cost.
var httpPrefilterMarkers = map[string][][]byte{
	"go": {
		[]byte("Handle"),  // HandleFunc, .Handle(
		[]byte("http."),   // http.Get/Post/NewRequest
		[]byte("router."), // gin/echo/chi router var
		[]byte("app."),    // fiber/gin app var
		[]byte("mux."),    // net/http mux
		[]byte(".GET("),   // fiber uppercase verbs
		[]byte(".POST("),
		[]byte(".PUT("),
		[]byte(".DELETE("),
		[]byte(".PATCH("),
	},
	"typescript": httpTsJsMarkers,
	"javascript": httpTsJsMarkers,
	"java":       httpJvmMarkers,
	"kotlin":     httpJvmMarkers,
	"dart": {
		[]byte("dio."), // lowercased dio instance
		[]byte("Dio."), // PascalCase Dio class
		[]byte("http."),
		[]byte("Router"),  // shelf_router `Router()` / `router.`
		[]byte("..get("),  // shelf_router cascade form
		[]byte("..post("), // idem
		[]byte("..put("),  // idem
		[]byte("..delete("),
		[]byte("..patch("),
		[]byte("..head("),
		[]byte("..options("),
	},
	// rust: the reqwest consumer pattern is `\w+\.(get|post|...)(`,
	// and those verbs are universal Rust method calls. Any marker
	// tight enough to reject non-HTTP files would also reject
	// reqwest consumers, so we leave rust out and run the regex
	// scan unconditionally.
	"csharp": {
		[]byte("[Http"),      // attribute routing
		[]byte(".Map"),       // minimal APIs
		[]byte("GetAsync("),  // HttpClient consumer idiom
		[]byte("PostAsync("), // idem
		[]byte("PutAsync("),
		[]byte("DeleteAsync("),
		[]byte("PatchAsync("),
	},
	"php": {
		[]byte("Route::"),
		[]byte("#[Route"),
		[]byte("->get("),
		[]byte("->post("),
		[]byte("->put("),
		[]byte("->delete("),
		[]byte("->patch("),
	},
}

var httpTsJsMarkers = [][]byte{
	[]byte("fetch("),
	[]byte("axios"),
	[]byte("@Get("),
	[]byte("@Post("),
	[]byte("@Put("),
	[]byte("@Delete("),
	[]byte("@Patch("),
	[]byte("@Head("),
	[]byte("@Options("),
	[]byte("app."),
	[]byte("router."),
}

var httpJvmMarkers = [][]byte{
	[]byte("Mapping"), // @GetMapping / @PostMapping / @RequestMapping
	[]byte("@Path"),   // JAX-RS
	[]byte("HttpClient"),
	[]byte("RestTemplate"),
	[]byte("WebClient"),
}

// Extract scans src for HTTP route patterns and returns contracts.
func (h *HTTPExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	lang := detectLanguage(filePath)
	if markers, ok := httpPrefilterMarkers[lang]; ok && !srcHasAnyMarker(src, markers) {
		return nil
	}

	text := string(src)
	lines := strings.Split(text, "\n")

	// Pre-sort file nodes by start line for enclosing-function lookup.
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})

	var out []Contract

	for _, pat := range httpPatterns {
		if !patternMatchesLang(pat, lang) {
			continue
		}
		for _, m := range pat.re.FindAllStringSubmatchIndex(text, -1) {
			lineNum := lineAtOffset(lines, m[0])
			method := pat.method
			path := ""

			if pat.methodGrp > 0 {
				method = strings.ToUpper(text[m[pat.methodGrp*2]:m[pat.methodGrp*2+1]])
			}
			path = text[m[pat.pathGrp*2]:m[pat.pathGrp*2+1]]

			normPath := NormalizeHTTPPath(path)
			contractID := fmt.Sprintf("http::%s::%s", method, normPath)

			symbolID := findEnclosingSymbol(fileNodes, lineNum)

			// Provider patterns that also capture the handler identifier
			// re-point SymbolID at the actual handler function in the
			// same file. Two forms handled:
			//   1. Bare handler:  r.GET("/users", listUsers)
			//      → handlerGrp captures "listUsers", resolve directly.
			//   2. Middleware-wrapped: mux.HandleFunc("POST /x",
			//      WithAuth(auth, h.CreateTuck)) — handlerGrp grabs
			//      "WithAuth" which is a wrapper. Walk forward from
			//      the end of the handlerGrp match, through the rest
			//      of the call's balanced parens, and pick the LAST
			//      identifier (or method reference like h.CreateTuck)
			//      that resolves to a function in this file. That's
			//      the innermost handler — what "trace a request"
			//      actually wants to land on.
			var handlerIdent, handlerTrail string
			if pat.handlerGrp > 0 && pat.role == RoleProvider {
				gStart := m[pat.handlerGrp*2]
				gEnd := m[pat.handlerGrp*2+1]
				if gStart >= 0 && gEnd > gStart {
					handlerName := text[gStart:gEnd]
					handlerIdent = handlerName
					// Always capture the full call-trail (every
					// argument between the HandleFunc parens) so a
					// later module-wide pass can enumerate handler
					// candidates — the narrow `\w+` regex capture
					// above stops at the first `.` in `h.ServeArchive`
					// and misses wrappers in `WithAuth(h.Foo)`.
					// callTrailSlice walks forward from the start of the
					// HandleFunc match to the matching `)`; passing m[0]
					// (match start) gets us the full args slice. Passing
					// m[1] (match end) would search past every paren we
					// care about and return the empty string.
					handlerTrail = callTrailSlice(text, m[0])
					if hID := resolveHandlerIdent(fileNodes, handlerName); hID != "" {
						symbolID = hID
					} else if hID := findInnermostResolvableHandler(fileNodes, handlerTrail); hID != "" {
						symbolID = hID
					}
				}
			}

			meta := map[string]any{
				"method":    method,
				"path":      normPath,
				"framework": pat.framework,
			}
			// Keep the raw handler identifier + the full call-trail
			// so a later module-wide pass can look handlers up
			// globally when file-scoped resolution failed. The
			// trail carries every candidate (wrappers + inner
			// handler) so we can pick the innermost-resolvable one
			// across repos.
			if handlerIdent != "" {
				meta["handler_ident"] = handlerIdent
			}
			if handlerTrail != "" {
				meta["handler_trail"] = handlerTrail
			}

			c := Contract{
				ID:         contractID,
				Type:       ContractHTTP,
				Role:       pat.role,
				SymbolID:   symbolID,
				FilePath:   filePath,
				Line:       lineNum,
				Meta:       meta,
				Confidence: pat.confidence,
			}

			// Second pass: pull request/response types, query params,
			// and status codes out of the handler body (provider) or
			// the call-site window (consumer). The enricher mutates
			// c.Meta in place and sets "schema_source".
			enrichHTTPContract(&c, lines, fileNodes, lang)

			out = append(out, c)
		}
	}

	return out
}

// detectLanguage infers the language from a file extension.
func detectLanguage(filePath string) string {
	switch {
	case strings.HasSuffix(filePath, ".go"):
		return "go"
	case strings.HasSuffix(filePath, ".ts"), strings.HasSuffix(filePath, ".tsx"):
		return "typescript"
	case strings.HasSuffix(filePath, ".js"), strings.HasSuffix(filePath, ".jsx"):
		return "javascript"
	case strings.HasSuffix(filePath, ".py"):
		return "python"
	case strings.HasSuffix(filePath, ".java"):
		return "java"
	case strings.HasSuffix(filePath, ".kt"), strings.HasSuffix(filePath, ".kts"):
		return "kotlin"
	case strings.HasSuffix(filePath, ".dart"):
		return "dart"
	case strings.HasSuffix(filePath, ".rs"):
		return "rust"
	case strings.HasSuffix(filePath, ".cs"):
		return "csharp"
	case strings.HasSuffix(filePath, ".rb"):
		return "ruby"
	case strings.HasSuffix(filePath, ".php"):
		return "php"
	case strings.HasSuffix(filePath, ".ex"), strings.HasSuffix(filePath, ".exs"):
		return "elixir"
	default:
		return ""
	}
}

// patternMatchesLang returns true if the pattern applies to the given language.
func patternMatchesLang(p httpPattern, lang string) bool {
	if len(p.languages) == 0 {
		return true
	}
	for _, l := range p.languages {
		if l == lang {
			return true
		}
	}
	return false
}

// lineAtOffset returns the 1-based line number for the given byte offset.
func lineAtOffset(lines []string, offset int) int {
	pos := 0
	for i, l := range lines {
		end := pos + len(l) + 1 // +1 for newline
		if offset < end {
			return i + 1
		}
		pos = end
	}
	return len(lines)
}

// filterFileNodes returns only nodes that belong to the given file.
func filterFileNodes(filePath string, nodes []*graph.Node) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n.FilePath == filePath {
			out = append(out, n)
		}
	}
	return out
}

// findEnclosingSymbol returns the ID of the nearest function/method that
// encloses the given line number.  Falls back to "" if none found.
//
// Strict containment (StartLine ≤ line ≤ EndLine) is preferred, but some
// language extractors (notably Dart's tree-sitter path) report EndLine as
// the signature line rather than the closing brace, so a call on the very
// next line wouldn't match. When strict containment fails, fall back to
// the closest-preceding symbol whose EndLine ≥ (line - closeProximity) —
// the call is most likely inside its body. "" still means nothing's even
// near enough.
func findEnclosingSymbol(sortedNodes []*graph.Node, line int) string {
	best := ""
	bestStart := 0
	for _, n := range sortedNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= line && n.EndLine >= line && n.StartLine >= bestStart {
			best = n.ID
			bestStart = n.StartLine
		}
	}
	if best != "" {
		return best
	}
	// Fallback: the closest function/method whose declaration precedes
	// the line — tolerates off-by-N EndLine reports from extractors that
	// don't compute the closing brace.
	fallback := ""
	fallbackStart := 0
	for _, n := range sortedNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= line && n.StartLine > fallbackStart {
			fallback = n.ID
			fallbackStart = n.StartLine
		}
	}
	return fallback
}

// findFunctionByName returns the ID of a function or method declared in the
// same file with the given short name (e.g. "listUsers"). Used by the HTTP
// provider extractor to re-point a contract's SymbolID at its handler
// function when the pattern captures it.
func findFunctionByName(fileNodes []*graph.Node, name string) string {
	for _, n := range fileNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.Name == name {
			return n.ID
		}
	}
	return ""
}

// resolveHandlerIdent resolves a handler identifier captured by a
// provider-pattern regex. Accepts bare "listUsers" (function name)
// and method-expression "h.CreateTuck" (dot-qualified) — the latter
// common when routes are registered on a receiver. The method-name
// after the dot is used for the lookup, so `h.CreateTuck` resolves
// to a method CreateTuck in the same file regardless of receiver
// variable name.
func resolveHandlerIdent(fileNodes []*graph.Node, ident string) string {
	if ident == "" {
		return ""
	}
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return findFunctionByName(fileNodes, ident)
}

// callTrailSlice returns the byte slice that starts at the HandleFunc
// call's opening "(" (found by the regex at matchStart) and ends at
// the matching balanced close ")". Used to scan past a middleware
// wrapper for an inner handler identifier. Returns empty when the
// call can't be balanced (which only happens on truncated or invalid
// source — production files are fine).
func callTrailSlice(src string, matchStart int) string {
	// Seek forward from matchStart to the first '(' — that's the
	// opening paren of the HandleFunc call. The regex's m[0] lands
	// at the start of the "HandleFunc" token.
	openIdx := -1
	for i := matchStart; i < len(src); i++ {
		if src[i] == '(' {
			openIdx = i
			break
		}
		if src[i] == '\n' {
			return ""
		}
	}
	if openIdx < 0 {
		return ""
	}
	depth := 0
	i := openIdx
	for i < len(src) {
		switch src[i] {
		case '(':
			depth++
			i++
		case ')':
			depth--
			if depth == 0 {
				return src[openIdx+1 : i]
			}
			i++
		case '"', '\'', '`':
			q := src[i]
			i++
			for i < len(src) && src[i] != q {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				i++
			}
			if i < len(src) {
				i++
			}
		default:
			i++
		}
	}
	return ""
}

// handlerCandidateRE captures every bare identifier or `recv.Method`
// style expression in the call-trail. Tight enough to skip keywords
// like "context" or "nil" only by not resolving them to a file-local
// function — the caller filters via findFunctionByName.
var handlerCandidateRE = regexp.MustCompile(`\b([A-Za-z_]\w*(?:\.\w+)?)\b`)

// HandlerCandidatesInTrail enumerates every identifier / receiver.method
// reference inside a HandleFunc call-trail in source order. The
// indexer's cross-file resolution pass uses this to pick the
// innermost-resolvable handler (last candidate that resolves to a
// real function or method globally).
func HandlerCandidatesInTrail(trail string) []string {
	matches := handlerCandidateRE.FindAllStringSubmatch(trail, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 && m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// findInnermostResolvableHandler walks the call trail and returns the
// LAST identifier that resolves to a function or method declared in
// the same file. For `WithAuth(auth, h.CreateTuck)` this is
// `h.CreateTuck` (resolves to CreateTuck method); WithAuth and auth
// fail to resolve (not file-local). Returns "" if no candidate
// resolves.
func findInnermostResolvableHandler(fileNodes []*graph.Node, trail string) string {
	matches := handlerCandidateRE.FindAllStringSubmatch(trail, -1)
	var best string
	for _, m := range matches {
		if id := resolveHandlerIdent(fileNodes, m[1]); id != "" {
			best = id
		}
	}
	return best
}
