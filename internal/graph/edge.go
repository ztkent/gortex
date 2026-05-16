package graph

type EdgeKind string

const (
	EdgeImports      EdgeKind = "imports"
	EdgeDefines      EdgeKind = "defines"
	EdgeCalls        EdgeKind = "calls"
	EdgeInstantiates EdgeKind = "instantiates"
	EdgeImplements   EdgeKind = "implements"
	EdgeExtends      EdgeKind = "extends"
	EdgeReferences   EdgeKind = "references"
	EdgeMemberOf     EdgeKind = "member_of"
	EdgeProvides     EdgeKind = "provides"
	EdgeConsumes     EdgeKind = "consumes"
	// EdgeMatches links a consumer contract node to the provider contract
	// node it resolves to (e.g. consumer http:GET:/v1/tucks → provider
	// http:GET:/v1/tucks, across repos). Traversals bridge service
	// boundaries by hopping Consumer → EdgeConsumes⁻¹ → consumer-contract
	// → EdgeMatches → provider-contract → EdgeProvides⁻¹ → handler.
	EdgeMatches EdgeKind = "matches"
	// EdgeAnnotated links a symbol to a synthetic annotation node
	// representing a decorator / annotation / attribute applied to it
	// (e.g. @Component, @Test, @Deprecated, #[derive(Debug)],
	// [Authorize], @app.route("/x"), @Published). The annotation node's
	// ID follows the convention "annotation::<lang>::<name>"; the edge's
	// Meta["args"] carries the verbatim argument text (truncated) when
	// the annotation has parentheses.
	//
	// Framework dispatch (NestJS @Get, Laravel middleware, Symfony
	// AsEventListener, Spring @Bean, FastAPI @app.route, …) continues
	// to flow through the contracts/dispatch layer with
	// EdgeProvides/EdgeConsumes — EdgeAnnotated runs in parallel as a
	// queryable record of the raw decorator. This lets agents answer
	// "find all @Deprecated" / "find all controllers" with one graph
	// hop without duplicating contract logic.
	EdgeAnnotated EdgeKind = "annotated"
	// EdgeTests links a test function/method to a non-test symbol it
	// exercises. Computed at index time as a post-extraction pass:
	// every call edge whose source is a test function (Meta["is_test"]
	// = true) and whose target is non-test produces an EdgeTests pair
	// alongside the existing EdgeCalls. Lets agents answer
	// "which tests cover X" with a single reverse-edge walk and lets
	// `get_untested_symbols` filter public symbols whose inverse-EdgeTests
	// set is empty.
	//
	// Test detection is by file naming convention plus per-language
	// fn-name conventions (Test*/Benchmark*/Fuzz* in Go, test_* /
	// Test* in Python, *_test.dart, etc.). Override per-repo via
	// .gortex.yaml::test_patterns when the project uses an unusual
	// layout — false positives are an acknowledged tradeoff for
	// keeping the heuristic dependency-free.
	EdgeTests EdgeKind = "tests"
	// EdgeReads / EdgeWrites split EdgeReferences for value-side uses
	// of variables and fields. LHS of an assignment / op= / ++ / --
	// emits EdgeWrites; every other identifier or selector use emits
	// EdgeReads. EdgeReferences is reserved for type references
	// (`var x SomeType` references the type SomeType) so the resolver
	// can keep distinguishing the two by target node kind.
	//
	// Together with KindField, these let agents ask "which functions
	// write to this field" — impossible with the previous "any use is
	// a reference" model. Implemented per-language as the Go/TS/
	// Python (priority wave) and Rust/Java (second wave) extractors
	// learn to walk assignment AST nodes.
	EdgeReads  EdgeKind = "reads"
	EdgeWrites EdgeKind = "writes"
	// EdgeThrows links a function/method to an error or exception
	// type that can propagate from it. Per language:
	//
	//   go      function returns an error type → edge to that type
	//           (custom *MyError type or external::error sentinel for
	//           the built-in error interface).
	//   python  `raise <Exception>` AST nodes inside the body.
	//   java    method `throws` clause.
	//   swift   `throws` / `rethrows` keyword on the function decl.
	//   rust    return type contains Result<_, E> → edge to E.
	//
	// Lets agents ask "what error types can propagate from here" with
	// a single forward walk and lets `analyze kind: "error_surface"`
	// summarise every public function's error contract without
	// re-deriving it from source.
	EdgeThrows EdgeKind = "throws"
	// Coverage edges: each is produced only when the relevant
	// index.coverage.<domain>.enabled gate is set; the registry is
	// permissive (DefaultOriginFor handles unknown kinds via the
	// confidence-score fallback).

	// EdgeParamOf links a KindParam node to its owning function or
	// method. Distinct from EdgeMemberOf (which is for fields of
	// types). Always ast_resolved by construction.
	EdgeParamOf EdgeKind = "param_of"
	// EdgeReturns links a function/method to a type it returns. Multi-
	// return Go functions emit one edge per result. Confidence reflects
	// the resolver: ast_inferred when the type is named in source,
	// promoted to ast_resolved / lsp_resolved by the semantic layer.
	EdgeReturns EdgeKind = "returns"
	// EdgeTypedAs binds a variable, parameter, field, or constant to
	// its declared type. Lets traversals answer "find all values of
	// type T". Distinct from EdgeReferences, which is broader.
	EdgeTypedAs EdgeKind = "typed_as"
	// EdgeCaptures links a closure node to an outer binding it closes
	// over.
	EdgeCaptures EdgeKind = "captures"
	// EdgeSpawns links a caller to a function it launches
	// asynchronously (goroutine, async/await, Promise, worker pool).
	// Emitted in addition to the corresponding EdgeCalls so synchronous
	// reachability queries can scope by edge kind. Meta["mode"] ∈
	// goroutine|async|promise|worker_pool.
	EdgeSpawns EdgeKind = "spawns"
	// EdgeSends / EdgeRecvs link a function to a channel-typed
	// variable for channel I/O. The channel's element type is reachable
	// via the variable's EdgeTypedAs edge.
	EdgeSends EdgeKind = "sends"
	EdgeRecvs EdgeKind = "recvs"
	// EdgeQueries links a function to a database table it queries
	// against. Default origin text_matched from string-literal SQL;
	// promoted to ast_resolved when an ORM mapping is recognized.
	EdgeQueries EdgeKind = "queries"
	// EdgeReadsCol / EdgeWritesCol provide column-level resolution
	// when the SQL parser can extract it. Falls back to table-level
	// EdgeQueries when columns can't be resolved.
	EdgeReadsCol  EdgeKind = "reads_col"
	EdgeWritesCol EdgeKind = "writes_col"
	// EdgeReadsConfig / EdgeWritesConfig link a function to a config
	// key it reads or writes (env var, viper key, k8s configmap entry,
	// struct-tag binding).
	EdgeReadsConfig  EdgeKind = "reads_config"
	EdgeWritesConfig EdgeKind = "writes_config"
	// EdgeTogglesFlag links a function to a feature flag it checks or
	// toggles. Meta["op"] ∈ read|write|register.
	EdgeTogglesFlag EdgeKind = "toggles_flag"
	// EdgeEmits links a function to a log/metric/trace event it emits.
	// Meta carries level (for logs), unit (for metrics), and label keys.
	//
	// EdgeEmits is also the publish side of the event pub/sub layer:
	// when a function publishes to a message broker (NATS / Kafka /
	// RabbitMQ / Redis pub-sub) or an in-process EventEmitter / Socket.IO
	// channel, the edge targets a KindEvent node with
	// Meta["event_kind"]="pubsub". The subscribe side is EdgeListensOn.
	// Meta["transport"] (nats|kafka|rabbitmq|redis|socketio|eventemitter|
	// unknown) and Meta["method"] (the matched call name) ride on the
	// edge so `analyze kind=pubsub` can group by broker without
	// re-deriving it.
	EdgeEmits EdgeKind = "emits"
	// EdgeListensOn links a subscriber/consumer function to the
	// KindEvent topic node it listens on — the read side of the event
	// pub/sub layer that parallels EdgeEmits' publish side. Emitted for
	// message-broker subscriptions (NATS Subscribe / Kafka consumer
	// subscribe / RabbitMQ Consume / Redis (P)Subscribe) and in-process
	// listener registration (EventEmitter.on / Socket.IO socket.on).
	// The target node always carries Meta["event_kind"]="pubsub";
	// Meta["transport"] and Meta["method"] ride on the edge. Origin:
	// ast_inferred — detection is a method-name + string-literal-topic
	// heuristic, not a type-checked fact, so it shares the tier the
	// observability extractor uses for EdgeEmits.
	EdgeListensOn EdgeKind = "listens_on"
	// EdgeGeneratedBy links a generated file to its schema source
	// (.proto, .graphql, openapi.yaml, etc.). Detected via comment
	// markers (// Code generated …), conventional adjacency, or
	// go:generate directives.
	EdgeGeneratedBy EdgeKind = "generated_by"
	// EdgeDependsOnModule links a file/package/import to a KindModule
	// node. One edge per import statement; aggregable to package-level.
	EdgeDependsOnModule EdgeKind = "depends_on_module"
	// EdgeOwns links a team to a file or directory. Sourced from
	// CODEOWNERS. Directory entries materialize per-file.
	EdgeOwns EdgeKind = "owns"
	// EdgeAuthored links a person/team to a node they last touched.
	// Meta carries commit and timestamp. People are stored as
	// KindTeam nodes with Meta["kind"]="person".
	EdgeAuthored EdgeKind = "authored"
	// EdgeCoveredBy links a function/method to a test that exercises
	// it, with coverage_pct attached in Meta. Directional inverse of
	// EdgeTests, distinguished by carrying the coverage metric.
	EdgeCoveredBy EdgeKind = "covered_by"
	// EdgeAliases links a type alias `type X = Y` to its underlying
	// type. Distinct from EdgeExtends (`type X Y` newtype) — agents
	// distinguish by edge kind to compute correct blast radius.
	EdgeAliases EdgeKind = "aliases"
	// EdgeComposes links a type to an embedded/composed/mixed-in type
	// (Go struct embedding, Rust trait bounds, Python multiple
	// inheritance). Distinct from EdgeExtends (newtype/inheritance/
	// interface extension).
	EdgeComposes EdgeKind = "composes"
	// EdgeOverrides links a method to the parent-class or interface
	// method it overrides. Distinct from EdgeImplements (interface
	// implementation) and EdgeExtends (class hierarchy) — those are
	// type-level relationships; EdgeOverrides is method-level. Emitted
	// alongside EdgeExtends/EdgeImplements when a child type declares
	// a method that shadows a parent method with the same signature.
	// Origin tier:
	//   lsp_resolved when the LSP server confirmed the override (e.g.
	//   tsserver / rust-analyzer / clangd report it via type hierarchy
	//   or its workspace symbol provider).
	//   ast_resolved when the parent type is in the same compilation
	//   unit and the indexer can prove the method exists in both.
	//   ast_inferred when the override is heuristic (same name only,
	//   parent type unknown).
	EdgeOverrides EdgeKind = "overrides"
	// EdgeLicensedAs links a file to its SPDX license. Sourced from
	// the file's SPDX-License-Identifier header, falling back to the
	// repo-level LICENSE file.
	EdgeLicensedAs EdgeKind = "licensed_as"
	// CPG-lite dataflow primitives. Together they form the data-
	// dependence layer Gortex layers on top of the call graph: agents
	// can answer "where does this value flow?" with a single graph
	// walk instead of hand-tracing source.
	//
	// Local-binding ID convention. For language extractors that emit
	// dataflow without materialising a graph node per local variable,
	// edges target a synthetic ID of the form:
	//
	//   <ownerID>#local:<name>@<line>
	//
	// where ownerID is the enclosing function/method/closure node.
	// These IDs are valid edge endpoints — BFS traverses them — but
	// no graph node is created, keeping search results free of
	// every transient binding in every function body.
	//
	// EdgeValueFlow links a value-producing position to a value-
	// consuming position within the same function/method/closure
	// body. Captures intra-procedural data-dependence: assignment
	// LHS↔RHS, range source↔induction var, return value↔function
	// symbol. Both endpoints are inside the same enclosing
	// function so the edge is fully resolved at extraction time.
	// Origin: ast_resolved by construction.
	// EdgeHandlesRoute links a handler function/method to the
	// KindContract node that represents its route. Emitted alongside
	// EdgeProvides whenever the contract type is HTTP/gRPC/WS/GraphQL/
	// topic — the framework layer that an agent asks "which symbol
	// serves /v1/users/:id?" about. The narrower edge kind lets
	// `analyze kind=routes` walk it without pulling in the broader
	// EdgeProvides graph (which also covers env keys, OpenAPI specs,
	// migrations, DI tokens, …). Origin tier mirrors the underlying
	// extractor; defaults to ast_resolved (structural by construction).
	EdgeHandlesRoute EdgeKind = "handles_route"
	// EdgeModelsTable links an ORM model type/class to the KindTable
	// node it persists. Per language:
	//
	//   go      struct with `gorm:"..."` tags or a `TableName() string`
	//           method on the receiver type
	//   python  SQLAlchemy / Django class with __tablename__ /
	//           class Meta: db_table
	//   ruby    class X < ApplicationRecord (or ActiveRecord::Base),
	//           with optional self.table_name = "..."
	//   java    @Entity class, optional @Table(name="...")
	//   typescript  TypeORM @Entity({ name: "..." }) class
	//
	// Lets agents ask "which table does this class write?" and "which
	// model owns the users table?" with a single graph hop instead of
	// joining through migrations and raw SQL.
	EdgeModelsTable EdgeKind = "models_table"
	// EdgeRendersChild links a parent component (function / method /
	// class) to a child component it renders inside its JSX/TSX/Vue/
	// Svelte template body. Captures the component dependency tree so
	// agents can ask "what renders <DataTable />?" or "what does
	// <CheckoutPage /> reach into?" without grepping for the
	// component name.
	//
	// Detection is heuristic: capital-first-letter element names
	// inside JSX expressions are treated as component references and
	// resolved through normal name resolution. Lowercase names map to
	// HTML/SVG primitives and are skipped — the edge graph would be
	// noise otherwise.
	EdgeRendersChild EdgeKind = "renders_child"
	EdgeValueFlow    EdgeKind = "value_flow"
	// EdgeArgOf links an argument expression at a call site to the
	// callee's parameter — the inter-procedural binding produced
	// by passing a value across a function boundary. Direction:
	// caller-side argument source → callee parameter. The
	// resolver lifts the unresolved callee target the same way
	// EdgeCalls is lifted; a follow-up indexer pass rewrites the
	// edge target from the callee function ID to the param node ID
	// at the recorded position (Meta["arg_position"]).
	EdgeArgOf EdgeKind = "arg_of"
	// EdgeReturnsTo links a callee function/method to the receiving
	// binding at a call site (`x := f(...)` produces returns_to(f, x)).
	// Direction: callee → assignment LHS. Stored at extraction time
	// with From = enclosing-caller ID and Meta["returns_to_call"] +
	// Meta["call_line"] as a placeholder; a follow-up indexer pass
	// rewrites From to the resolved callee ID by joining against the
	// EdgeCalls edge from the same caller+line.
	EdgeReturnsTo EdgeKind = "returns_to"
	// Infrastructure-graph edges. Materialised by the K8s
	// manifest, Kustomize, and Dockerfile extractors.
	//
	// EdgeConfigures links a workload Resource (Pod / Deployment /
	// StatefulSet / DaemonSet / Job / CronJob) to a ConfigMap or
	// Secret it pulls configuration from via `envFrom:`,
	// `valueFrom: configMapKeyRef`, or `valueFrom: secretKeyRef`.
	// Direction: consumer → provider (workload → ConfigMap/Secret).
	// Origin: ast_resolved by construction.
	EdgeConfigures EdgeKind = "configures"
	// EdgeMounts links a workload Resource to a volume source —
	// ConfigMap, Secret, or PersistentVolumeClaim — referenced from
	// `spec.volumes`. Direction: workload → volume source. Distinct
	// from EdgeConfigures (which is env-side wiring); EdgeMounts is
	// the filesystem-side wiring. Origin: ast_resolved.
	EdgeMounts EdgeKind = "mounts"
	// EdgeExposes links a Resource or Image to a port surface it
	// publishes. Source: K8s Service `spec.ports[]`, Deployment/Pod
	// `containerPorts[]`, Ingress rules, Dockerfile `EXPOSE`. Target:
	// a synthetic port node with ID `port::<proto>::<n>` (proto ∈
	// tcp|udp|http|https|grpc). Origin: ast_resolved.
	EdgeExposes EdgeKind = "exposes"
	// EdgeDependsOn captures runtime/build dependencies between
	// infrastructure entities — Ingress → Service backend, Service
	// → Pod (selector), Kustomization → base Kustomization,
	// Dockerfile stage → parent stage / external base Image,
	// Resource → Image. Direction: dependent → dependency. Origin:
	// ast_resolved.
	//
	// Also the model-lineage edge for the dbt / SQLMesh graph layer:
	// a dbt model → the models / seeds / snapshots it `ref()`s and the
	// sources it `source()`s, and a SQLMesh model → the models its
	// body reads via FROM / JOIN. Direction is the same (dependent →
	// dependency), so a downstream-impact walk over EdgeDependsOn
	// answers "what breaks if this model changes?" uniformly across
	// infra manifests and the transformation DAG. Edge Meta["link"]
	// disambiguates (ref|source|from).
	EdgeDependsOn EdgeKind = "depends_on"
	// EdgeUsesEnv links a Resource (workload) or Image (Dockerfile
	// stage) to a KindConfigKey representing an environment variable
	// it declares it needs at runtime. Direction: container surface
	// → config_key. The config_key ID convention `cfg::env::<NAME>`
	// matches what Go / Python / Node extractors emit for
	// `os.Getenv("NAME")` (and equivalents) so the cross-ref between
	// infra-side declaration and code-side consumption materialises
	// for free via shared node IDs. Origin: ast_resolved.
	EdgeUsesEnv EdgeKind = "uses_env"
	// EdgeSimilarTo links two function/method nodes whose bodies are
	// near-duplicates ("clones"). Materialised by the graph-wide
	// MinHash + LSH clone-detection pass: each function body is reduced
	// to a 64-slot MinHash signature at index time (stored on
	// Node.Meta["clone_sig"]), LSH banding produces candidate pairs,
	// and a Jaccard-similarity threshold filter keeps the true clones.
	// Emitted symmetrically — both fA→fB and fB→fA — so "what are the
	// clones of X" is a single out-edge walk from either endpoint.
	// Meta["similarity"] carries the estimated Jaccard score (0..1);
	// Confidence mirrors it. Origin: ast_inferred — the relationship is
	// a statistical estimate over normalised tokens, not a structural
	// fact. Pairs with dead-code analysis to surface "dead duplicates
	// of live code" — a near-duplicate of a live function that itself
	// has zero callers.
	EdgeSimilarTo EdgeKind = "similar_to"
	// Cross-repo edge kinds. Materialised by the resolver's
	// detectCrossRepoEdges pass: whenever a calls / implements / extends
	// edge has a From node and a To node in two different repos, a
	// parallel edge of the matching cross_repo_* kind is emitted
	// alongside the base edge (the base edge keeps its kind and also
	// gets Edge.CrossRepo set). The narrower kinds let
	// `analyze kind=cross_repo` walk only the repo-boundary-crossing
	// subset of the call / type-hierarchy graph without re-deriving the
	// boundary test from node RepoPrefix on every edge. From/To/FilePath/
	// Line/Origin/Confidence mirror the base edge; Origin is therefore
	// inherited (lsp_resolved for an LSP-confirmed call, ast_resolved
	// for a structural implements/extends, etc.). Idempotent —
	// graph.AddEdge dedupes by edgeKey — and incremental-safe — EvictFile
	// removes a node's edges in both directions, so a stale parallel
	// edge cannot survive a reindex of either endpoint's file.
	EdgeCrossRepoCalls      EdgeKind = "cross_repo_calls"
	EdgeCrossRepoImplements EdgeKind = "cross_repo_implements"
	EdgeCrossRepoExtends    EdgeKind = "cross_repo_extends"
)

// CrossRepoKindFor maps a base edge kind to its parallel cross-repo
// variant. Only the call / type-hierarchy kinds named in M3 have a
// variant; every other kind returns ok == false. The mapping is the
// single source of truth for both the resolver's detectCrossRepoEdges
// pass and `analyze kind=cross_repo`.
func CrossRepoKindFor(base EdgeKind) (EdgeKind, bool) {
	switch base {
	case EdgeCalls:
		return EdgeCrossRepoCalls, true
	case EdgeImplements:
		return EdgeCrossRepoImplements, true
	case EdgeExtends:
		return EdgeCrossRepoExtends, true
	}
	return "", false
}

// BaseKindForCrossRepo is the inverse of CrossRepoKindFor: it maps a
// cross-repo edge kind back to the base relation it parallels. Returns
// ok == false for any non-cross-repo kind.
func BaseKindForCrossRepo(cr EdgeKind) (EdgeKind, bool) {
	switch cr {
	case EdgeCrossRepoCalls:
		return EdgeCalls, true
	case EdgeCrossRepoImplements:
		return EdgeImplements, true
	case EdgeCrossRepoExtends:
		return EdgeExtends, true
	}
	return "", false
}

type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     EdgeKind `json:"kind"`
	FilePath string   `json:"file_path"`
	Line     int      `json:"line"`
	// Confidence is the numeric score (0..1). Kept on the in-memory
	// struct for internal filtering (min_tier, etc.) but excluded from
	// JSON — agents act on ConfidenceLabel, and the float adds ~15
	// chars to every edge in large graph responses.
	Confidence      float64 `json:"-"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
	Origin          string  `json:"origin,omitempty"`
	// Tier is the coarse provenance label derived from Origin
	// (ast / lsp / heuristic). It is the agent-facing summary used by
	// retrieval UIs and competitor-parity columns (tokensave's
	// edges.resolved_by). Populated by enrichSubGraphEdges and the
	// dataflow encoders; empty by default on the in-memory edge.
	Tier      string `json:"tier,omitempty"`
	CrossRepo bool   `json:"cross_repo,omitempty"`
	// Meta is intentionally excluded from JSON. It holds internal
	// instrumentation (semantic_source, provider hints, etc.) that agents
	// don't consume but that adds measurable bytes to every edge in
	// responses returning hundreds of call-graph edges. Internal callers
	// can still read/write the field; external MCP consumers don't see it.
	Meta map[string]any `json:"-"`
}

// Edge.Origin values — call-graph confidence tiers, highest → lowest. Use
// MeetsMinTier / OriginRank to compare.
//
//   - lsp_resolved: Compiler-grade. LSP, go/types, or SCIP confirms that this
//     edge's target is the precise symbol being referenced. Safe to rely on
//     for refactors.
//   - lsp_dispatch: Interface → implementation dispatch resolved by a
//     semantic provider. One step less direct than a literal target match.
//   - ast_resolved: Tree-sitter / AST extraction found a unique target in
//     the same compilation unit. No type system involved, but structurally
//     unambiguous.
//   - ast_inferred: Heuristic resolution using type info we extracted from
//     the AST. Not compiler-verified.
//   - text_matched: Name-only match. The weakest tier — could be a false
//     positive.
const (
	OriginLSPResolved = "lsp_resolved"
	OriginLSPDispatch = "lsp_dispatch"
	OriginASTResolved = "ast_resolved"
	OriginASTInferred = "ast_inferred"
	OriginTextMatched = "text_matched"
)

// OriginRank returns a numeric rank for origin comparison. Higher = more
// confident. Unknown or empty origin returns 0 so it sorts below all known
// tiers; filters treat it as "untagged" and fall back to legacy inference.
func OriginRank(origin string) int {
	switch origin {
	case OriginLSPResolved:
		return 5
	case OriginLSPDispatch:
		return 4
	case OriginASTResolved:
		return 3
	case OriginASTInferred:
		return 2
	case OriginTextMatched:
		return 1
	}
	return 0
}

// MeetsMinTier returns true when origin is at least as confident as minTier.
// Empty minTier always passes (no filter). Empty origin fails any non-empty
// filter — callers wanting legacy fallback should first backfill via
// DefaultOriginFor.
func MeetsMinTier(origin, minTier string) bool {
	if minTier == "" {
		return true
	}
	return OriginRank(origin) >= OriginRank(minTier)
}

// ResolvedBy maps an Origin tier to a coarse provenance label used by
// agent retrieval UIs and competitor parity rows (cf. tokensave's
// `edges.resolved_by` column). The mapping collapses the five Origin
// tiers to three buckets:
//
//   - "lsp":       compiler-grade evidence (OriginLSPResolved / Dispatch)
//   - "ast":       structurally-unambiguous AST extraction
//   - "heuristic": name- or score-derived guess (ast_inferred / text_matched)
//
// Empty origin or unknown values return "heuristic" — a safe fallback
// for back-compat with graphs produced before Origin was stamped.
func ResolvedBy(origin string) string {
	switch origin {
	case OriginLSPResolved, OriginLSPDispatch:
		return "lsp"
	case OriginASTResolved:
		return "ast"
	case OriginASTInferred, OriginTextMatched:
		return "heuristic"
	}
	return "heuristic"
}

// DefaultOriginFor derives an origin tier for edges that don't have Origin
// set yet (edges from providers not updated to set Origin directly, or from
// indexes produced before this field existed). Uses edge kind, confidence
// score, and semantic_source meta as fallback signals. Never returns empty.
func DefaultOriginFor(kind EdgeKind, confidence float64, semanticSource string) string {
	if semanticSource != "" {
		if kind == EdgeImplements || kind == EdgeOverrides {
			return OriginLSPDispatch
		}
		return OriginLSPResolved
	}
	// Structural AST edges are unambiguous by construction.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf,
		EdgeImplements, EdgeProvides, EdgeConsumes, EdgeMatches,
		// Coverage structural edges: the extractor produces an
		// unambiguous source→target binding for each, so they share
		// the AST-resolved tier.
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeOverrides, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgeCaptures,
		// Framework-layer edges. Each is materialised by an extractor
		// that already proved the relationship (handler → route via
		// the contracts pipeline, model → table via the ORM detector,
		// parent → child via JSX walking) so they ride at ast_resolved.
		EdgeHandlesRoute, EdgeModelsTable, EdgeRendersChild,
		// Dataflow edges. EdgeValueFlow is intra-procedural and
		// fully resolved at extraction. EdgeArgOf / EdgeReturnsTo
		// inherit ast_resolved once the post-resolution pass has
		// landed both ends; the dispatcher here just stamps the
		// default tier so freshly emitted edges classify cleanly.
		EdgeValueFlow, EdgeArgOf, EdgeReturnsTo,
		// Infrastructure-graph edges. Each is materialised by an
		// extractor (K8s/Kustomize/Dockerfile) that resolves the
		// relationship structurally from the manifest text.
		EdgeConfigures, EdgeMounts, EdgeExposes, EdgeDependsOn, EdgeUsesEnv,
		// Cross-repo type-hierarchy edges parallel the structural
		// implements/extends edges, so they ride at the same tier.
		// EdgeCrossRepoCalls is intentionally excluded — it parallels
		// the resolution-derived `calls` edge and inherits that tier
		// via the confidence fallback below.
		EdgeCrossRepoImplements, EdgeCrossRepoExtends:
		return OriginASTResolved
	}
	// Resolution-derived edges fall back to confidence score.
	switch {
	case confidence >= 0.9:
		return OriginASTResolved
	case confidence >= 0.5:
		return OriginASTInferred
	}
	return OriginTextMatched
}

// ConfidenceLabelFor returns EXTRACTED, INFERRED, or AMBIGUOUS for an edge
// based on its kind and confidence value.
//
// Kept for back-compat; new code should prefer Origin tiers (OriginRank /
// MeetsMinTier) which distinguish LSP-grade from AST-grade evidence.
func ConfidenceLabelFor(kind EdgeKind, confidence float64) string {
	// Structural edges from AST are always extracted.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf, EdgeImplements,
		EdgeProvides, EdgeConsumes, EdgeMatches,
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeOverrides, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgeCaptures,
		// Framework-layer edges. Each is materialised by an extractor
		// that already proved the relationship (handler → route via
		// the contracts pipeline, model → table via the ORM detector,
		// parent → child via JSX walking) so they ride at ast_resolved.
		EdgeHandlesRoute, EdgeModelsTable, EdgeRendersChild,
		EdgeValueFlow, EdgeArgOf, EdgeReturnsTo,
		// Infrastructure-graph edges (K8s / Kustomize / Dockerfile).
		EdgeConfigures, EdgeMounts, EdgeExposes, EdgeDependsOn, EdgeUsesEnv,
		// Cross-repo type-hierarchy edges parallel structural
		// implements/extends; EdgeCrossRepoCalls falls through to the
		// confidence-score classifier like the base `calls` edge.
		EdgeCrossRepoImplements, EdgeCrossRepoExtends:
		return "EXTRACTED"
	}
	// Resolution-derived edges: classify by confidence score.
	switch {
	case confidence >= 0.9:
		return "EXTRACTED"
	case confidence >= 0.5:
		return "INFERRED"
	case confidence > 0:
		return "AMBIGUOUS"
	default:
		// confidence == 0 means resolved without type info.
		return "INFERRED"
	}
}
