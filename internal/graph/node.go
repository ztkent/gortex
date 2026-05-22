package graph

import "strings"

type NodeKind string

const (
	KindFile      NodeKind = "file"
	KindPackage   NodeKind = "package"
	KindFunction  NodeKind = "function"
	KindMethod    NodeKind = "method"
	KindType      NodeKind = "type"
	KindInterface NodeKind = "interface"
	KindVariable  NodeKind = "variable"
	KindImport    NodeKind = "import"
	KindContract  NodeKind = "contract"
	// KindField represents a struct field, class property, or record
	// field — anything addressable as `owner.field`. ID convention:
	// `<file>::<owner>.<field>`. EdgeMemberOf links the field to its
	// owning type. Languages that already emitted class properties as
	// KindVariable (TypeScript, PHP) keep doing so for backwards
	// compatibility — KindField is reserved for languages that
	// previously emitted only type-ref edges from fields (Go, Rust,
	// Java, C#).
	KindField NodeKind = "field"
	// Coverage kinds: each is gated behind a per-domain
	// .gortex.yaml::index.coverage.<domain>.enabled. Parsers register a
	// kind on first use; the registry is permissive (validNodeKinds
	// accepts all known kinds) so an unenabled domain simply produces no
	// nodes of that kind, rather than failing extraction.

	// KindParam represents a single function/method parameter. ID
	// convention: `<func-id>#param:<name>`. EdgeParamOf links the param
	// node back to its owner; EdgeTypedAs binds it to its declared
	// type. Created when index.function_shape.enabled is true.
	KindParam NodeKind = "param"
	// KindClosure represents an anonymous function / lambda inside an
	// enclosing function. ID convention: `<file>::<enclosing>#closure@<line>`.
	// Calls/reads/writes inside the closure attribute to the closure
	// node, not its enclosing function. EdgeMemberOf links to the
	// enclosing function. EdgeCaptures lists outer bindings closed over.
	KindClosure NodeKind = "closure"
	// KindConstant peels off `const`, `iota`, top-level immutable
	// bindings, and language-specific constant declarations from
	// KindVariable. Existing variable-kind nodes are re-classified on
	// next index; IDs are preserved.
	KindConstant NodeKind = "constant"
	// KindEnumMember represents one member of an enum-like type. ID
	// convention: `<file>::<EnumType>.<Member>`. EdgeMemberOf links to
	// the enum's type node.
	KindEnumMember NodeKind = "enum_member"
	// KindGenericParam represents a type parameter declared by a
	// function or type. ID convention: `<owner-id>#tparam:<name>`.
	KindGenericParam NodeKind = "generic_param"
	// KindModule represents a single (ecosystem, name, version) tuple
	// for an external dependency. Shared across files that import it.
	// ID convention: `module::<ecosystem>:<name>@<version>`.
	// Ecosystems: go, npm, pypi, cargo, maven, composer, gem, hex, nuget.
	KindModule NodeKind = "module"
	// KindTable represents a database table. ID convention:
	// `db::<dialect>::<schema>.<table>`. Sourced from migrations, ORM
	// models, and string-literal SQL in priority order.
	KindTable NodeKind = "table"
	// KindColumn represents a database column. ID convention:
	// `db::<dialect>::<schema>.<table>.<column>`. EdgeMemberOf links to
	// the owning table.
	KindColumn NodeKind = "column"
	// KindConfigKey represents a configuration key — env var, viper
	// path, CLI flag, struct-tag-driven field, or k8s ConfigMap entry.
	// ID convention: `cfg::<source>::<dotted.path>`. Source ∈
	// env|viper|flags|k8s_cm|k8s_secret|struct_tag.
	KindConfigKey NodeKind = "config_key"
	// KindFlag represents a feature flag / experiment. ID convention:
	// `flag::<provider>::<name>`. Provider ∈ growthbook|launchdarkly|
	// unleash|internal|env.
	KindFlag NodeKind = "flag"
	// KindEvent represents a log, metric, span, or trace name emitted
	// from code, or a pub/sub topic/channel/subject. ID convention:
	// `event::<kind>::<name>` for observability events and
	// `event::pubsub::<transport>::<name>` for pub/sub topics.
	// Meta["event_kind"] ∈ log|metric|trace|span|pubsub. For pubsub
	// events Meta["transport"] ∈ nats|kafka|rabbitmq|redis|socketio|
	// eventemitter|unknown; publishers link in via EdgeEmits and
	// subscribers via EdgeListensOn.
	KindEvent NodeKind = "event"
	// KindMigration represents a database migration unit. ID
	// convention: `migration::<dialect>::<id>`. Provides tables/columns
	// it creates; consumes ones it references.
	KindMigration NodeKind = "migration"
	// KindFixture represents a test data file or golden file. ID
	// convention: `fixture::<path>`. Test functions reference it via
	// EdgeReferences.
	KindFixture NodeKind = "fixture"
	// KindTodo represents a TODO/FIXME/HACK/XXX/NOTE comment marker. ID
	// convention: `todo::<file>:<line>`. Meta carries tag, assignee,
	// due, ticket, and the truncated text.
	KindTodo NodeKind = "todo"
	// KindTeam represents a CODEOWNERS team or individual. ID
	// convention: `team::<name>`. Meta.kind ∈ team|person disambiguates.
	KindTeam NodeKind = "team"
	// KindRelease represents a tag/version boundary. ID convention:
	// `release::<tag>`. Used as a query filter via Node.Meta["added_in"]
	// rather than as an edge endpoint in most cases.
	KindRelease NodeKind = "release"
	// KindLicense represents an SPDX license identifier. ID convention:
	// `license::<spdx>`. Files link to it via EdgeLicensedAs.
	KindLicense NodeKind = "license"
	// KindString represents a string literal that crosses an API
	// boundary worth tracking — Datadog/Prometheus metric names,
	// errors.New / fmt.Errorf messages, raw HTTP route paths, and
	// (later) HTML class/id values. ID convention:
	// `string::<context>::<value-or-hash>`. Context ∈
	// metric|error_msg|route|html_class|html_id|… EdgeEmits links the
	// enclosing function/method to the string node, mirroring KindEvent.
	// Per-repo: applyRepoPrefix prefixes every node ID with the repo
	// slug so two repos that emit the same string don't collide.
	KindString NodeKind = "string"
	// KindResource represents a Kubernetes manifest resource —
	// Deployment, Service, ConfigMap, Secret, Ingress, CronJob,
	// StatefulSet, DaemonSet, Job, ReplicaSet, ServiceAccount,
	// Role, RoleBinding, ClusterRole, ClusterRoleBinding, Namespace,
	// PersistentVolume, PersistentVolumeClaim, etc. ID convention:
	// `k8s::<kind>::<namespace>::<name>` (namespace defaults to
	// "_" when not declared in the manifest). Meta carries
	// api_version, namespace, labels (truncated). Sourced from YAML
	// extractors that detect K8s manifests by `apiVersion:` +
	// `kind:` markers.
	KindResource NodeKind = "resource"
	// KindKustomization represents a Kustomize overlay — one per
	// `kustomization.yaml` / `kustomization.yml` file in a repo.
	// ID convention: `kustomize::<dir>` where dir is the directory
	// holding the kustomization file relative to the repo root.
	// Resources, bases, components, and patches are linked via
	// EdgeDependsOn (overlay → base) and EdgeReferences
	// (overlay → resource files).
	KindKustomization NodeKind = "kustomization"
	// KindImage represents a container image — either an external
	// base image referenced by a Dockerfile FROM, or a build stage
	// declared via `FROM ... AS <stage>`, or an image referenced
	// by a K8s container spec. ID conventions:
	//   `image::<name>:<tag>` for external/registry images (tag
	//     defaults to "latest" when omitted)
	//   `image::stage::<file>::<stage-name>` for Dockerfile build
	//     stages
	// Meta carries registry, digest (when pinned), platform.
	KindImage NodeKind = "image"
	// KindArtifact represents a non-code knowledge file declared in
	// the `.gortex.yaml::artifacts` manifest — a DB schema (SQL /
	// Prisma / dbt), an API spec (OpenAPI / GraphQL / protobuf), an
	// infra config (Terraform / Kustomize / Helm), or an
	// architecture doc (ADR markdown). ID convention:
	// `artifact::<repo-relative-path>`. Meta carries artifact_kind
	// (schema|api|infra|doc), content_hash (sha256 of the file —
	// drives staleness detection), title, and size. The artifact
	// node links to every symbol it mentions via EdgeReferences so
	// agents can pull the right schema or spec alongside the code.
	KindArtifact NodeKind = "artifact"
	// KindDoc represents one heading-delimited prose section of a
	// documentation file (Markdown). Name is the breadcrumb heading
	// path ("README.md > Setup > Build"); Meta["section_text"] holds
	// the section's paragraph text with markdown syntax stripped, and
	// the BM25 search index is fed that body so a prose query ranks
	// the right section. ID convention:
	// "<file>::doc:<slug-of-heading-path>" -- derived from the
	// heading path, NOT line numbers, so an incremental reindex of an
	// edited file keeps stable section identity. The owning file
	// links to it via EdgeDefines.
	KindDoc NodeKind = "doc"
)

var validNodeKinds = map[NodeKind]bool{
	KindFile: true, KindPackage: true, KindFunction: true,
	KindMethod: true, KindType: true, KindInterface: true,
	KindVariable: true, KindImport: true, KindContract: true,
	KindField: true,
	// Coverage kinds — see Kind* doc comments above for usage notes.
	KindParam: true, KindClosure: true, KindConstant: true,
	KindEnumMember: true, KindGenericParam: true, KindModule: true,
	KindTable: true, KindColumn: true, KindConfigKey: true,
	KindFlag: true, KindEvent: true, KindMigration: true,
	KindFixture: true, KindTodo: true, KindTeam: true,
	KindRelease: true, KindLicense: true, KindString: true,
	KindResource: true, KindKustomization: true, KindImage: true,
	KindArtifact: true, KindDoc: true,
}

type Node struct {
	ID        string   `json:"id"`
	Kind      NodeKind `json:"kind"`
	Name      string   `json:"name"`
	QualName  string   `json:"qual_name,omitempty"`
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"`
	// EndLine is omitted when zero — File-kind nodes don't have ranges.
	EndLine    int            `json:"end_line,omitempty"`
	Language   string         `json:"language"`
	Meta       map[string]any `json:"meta,omitempty"`
	RepoPrefix string         `json:"repo_prefix,omitempty"`
	// WorkspaceID is the hard graph boundary slug. Two nodes with
	// different WorkspaceIDs are not allowed to be matched as contract
	// provider/consumer pairs and queries scope by it by default.
	// Defaults at warmup time to the per-repo `.gortex.yaml::workspace`
	// setting; falls back to RepoPrefix when no workspace is declared
	// (so old configs keep working) and to "" only for snapshot
	// records written before the field existed (gob decodes unknown
	// fields as zero — warmup backfills these from config).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID is the soft sub-boundary inside a workspace. One
	// project per repo by default; monorepos can declare projects[] in
	// .gortex.yaml. Contract pairing is bounded to a single
	// (workspace_id, project_id); cross-project contracts become orphans.
	// Defaults to the repo name when no projects[] mapping matches.
	ProjectID string `json:"project_id,omitempty"`
	// AbsoluteFilePath is the on-disk absolute path corresponding to
	// FilePath. It is empty on the canonical graph node and is populated
	// only on the per-response copies the MCP layer hands to result
	// encoders, so an editor or agent can open a result directly without
	// reconstructing the path from repo_prefix + file_path.
	AbsoluteFilePath string `json:"absolute_file_path,omitempty"`
}

// Brief returns a compact representation with only the fields needed for listing.
func (n *Node) Brief() map[string]any {
	b := map[string]any{
		"id":         n.ID,
		"name":       n.Name,
		"kind":       n.Kind,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
	if n.RepoPrefix != "" {
		b["repo_prefix"] = n.RepoPrefix
	}
	if n.WorkspaceID != "" {
		b["workspace_id"] = n.WorkspaceID
	}
	if n.ProjectID != "" {
		b["project_id"] = n.ProjectID
	}
	// Surface visibility and a short doc snippet when present — Brief
	// is the listing projection used by search_symbols and find_usages,
	// where these two fields meaningfully sharpen the result so the
	// agent can decide without a follow-up get_symbol_source call.
	if v, ok := n.Meta["visibility"].(string); ok && v != "" {
		b["visibility"] = v
	}
	if d, ok := n.Meta["doc"].(string); ok && d != "" {
		// Truncate doc to 80 chars in Brief — the full doc is on the
		// node, this is just the listing teaser.
		const briefDocCap = 80
		if len(d) > briefDocCap {
			d = d[:briefDocCap] + "…"
		}
		b["doc"] = d
	}
	// Test classification — stamped by the indexer's test-edge pass.
	// Surfacing it on the listing row lets agents tell production
	// callers from test callers without a follow-up call.
	if v, ok := n.Meta["is_test"].(bool); ok && v {
		b["is_test"] = true
	}
	if r, ok := n.Meta["test_role"].(string); ok && r != "" {
		b["test_role"] = r
	}
	if r, ok := n.Meta["test_runner"].(string); ok && r != "" {
		b["test_runner"] = r
	}
	if v, ok := n.Meta["is_test_file"].(bool); ok && v {
		b["is_test_file"] = true
	}
	// A prose-section node carries no signature -- surface a short
	// snippet of its body text so a docs search result is
	// self-describing without a follow-up read.
	if n.Kind == KindDoc {
		if txt, ok := n.Meta["section_text"].(string); ok && txt != "" {
			const snippetCap = 160
			if len(txt) > snippetCap {
				txt = txt[:snippetCap] + "\u2026"
			}
			b["section"] = txt
		}
	}
	// enclosing / enclosing_id name the symbol this node is declared
	// inside -- the receiver type of a method, the struct of a field,
	// the enum of a member, the function around a closure. Derived
	// from the ID convention; absent for top-level symbols. Lets a
	// search result say "Parse on type Decoder" without a follow-up
	// call.
	if eid, ename := EnclosingFromID(n.ID, n.Kind); ename != "" {
		b["enclosing"] = ename
		b["enclosing_id"] = eid
	}
	// AbsoluteFilePath is populated only on the per-response copies the
	// MCP layer builds (see Server.withAbsPaths); empty on canonical nodes.
	if n.AbsoluteFilePath != "" {
		b["absolute_file_path"] = n.AbsoluteFilePath
	}
	return b
}

// EnclosingFromID derives a node's enclosing owner purely from its
// ID and kind -- no graph access. It covers the kinds whose ID
// convention embeds the owner:
//
//   - method  "<file>::<Owner>.<method>"      -> owner "<file>::<Owner>"
//   - field   "<file>::<owner>.<field>"       -> owner "<file>::<owner>"
//   - enum    "<file>::<EnumType>.<Member>"   -> owner "<file>::<EnumType>"
//   - closure "<file>::<enclosing>#closure@N" -> owner "<file>::<enclosing>"
//
// For every other kind -- and for a method/field/closure whose ID
// carries no owner segment -- both return values are empty. The
// returned name is the owner's short (last-segment) name.
//
// This is the standalone derivation Node.Brief uses; callers with a
// graph reader should prefer the richer EdgeMemberOf-based lookup,
// which also resolves owners the ID does not name.
func EnclosingFromID(id string, kind NodeKind) (ownerID, ownerName string) {
	sep := strings.Index(id, "::")
	if sep < 0 {
		return "", ""
	}
	file, symbol := id[:sep], id[sep+2:]
	switch kind {
	case KindClosure:
		// "<enclosing>#closure@<line>" -- the owner is the segment
		// before the first '#'.
		if h := strings.IndexByte(symbol, '#'); h > 0 {
			owner := symbol[:h]
			return file + "::" + owner, lastIDSegment(owner)
		}
		return "", ""
	case KindMethod, KindField, KindEnumMember:
		// "<Owner>.<member>" -- the owner is everything before the
		// last '.'.
		if dot := strings.LastIndexByte(symbol, '.'); dot > 0 {
			owner := symbol[:dot]
			return file + "::" + owner, lastIDSegment(owner)
		}
		return "", ""
	default:
		return "", ""
	}
}

// lastIDSegment returns the last dotted segment of an identifier --
// its human-facing short name.
func lastIDSegment(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// EnclosingShortName returns the human-facing short name of an
// owner identifier or node ID -- its last "::"- or "."-separated
// segment. Used when only an owner ID string is in hand and no node
// was resolved.
func EnclosingShortName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return lastIDSegment(s)
}

func ValidNodeKind(k NodeKind) bool {
	return validNodeKinds[k]
}
