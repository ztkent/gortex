package languages

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Ansible playbook / role extraction.
//
// Ansible YAML is structurally a top-level SEQUENCE, which is what
// separates it from the mapping-rooted sub-formats (K8s manifests, dbt
// schema files) handled elsewhere in the YAML dispatch chain. Two file
// shapes are recognised:
//
//   - playbook   — a sequence of PLAY mappings, each carrying `hosts:`
//                  (or an `import_playbook:` directive). Each play groups
//                  tasks (`tasks:` / `pre_tasks:` / `post_tasks:`),
//                  handlers (`handlers:`), and role references (`roles:`).
//   - tasks file — a sequence of TASK mappings under a role's `tasks/`
//                  directory; likewise a `handlers/` file is a sequence
//                  of HANDLER mappings. These have no `hosts:`; the file
//                  path (`/tasks/`, `/handlers/`, `/roles/`) plus the
//                  task fingerprint (a module key alongside `name:`) is
//                  the signal.
//
// Graph shape. The file node (emitted by the host YAMLExtractor) is the
// playbook. Each play becomes a KindType node; each task / handler
// becomes a KindFunction node. The module a task invokes is recorded as
// an EdgeCalls to an unresolved `ansible_module` target so module usage
// is a single reverse-edge walk. `notify:` and `roles:` become
// EdgeReferences to unresolved `ansible_handler` / `ansible_role`
// targets, resolved by name. IDs are deterministic and name-derived:
//
//   <file>::play:<name>       a play (name → `name:` | hosts | play-<i>)
//   <file>::task:<name>       a task (name → `name:` | <module>-<i>)
//   <file>::handler:<name>    a handler (name → `name:` | <module>-<i>)
//
// All emitted node / edge kinds are existing graph kinds; the Ansible
// role is carried in Meta["ansible_kind"].

// ansibleTaskDirectives is the set of task-level keys that are *not* a
// module invocation. The single remaining key in a task mapping (after
// these are excluded) is the module the task runs.
var ansibleTaskDirectives = map[string]bool{
	"name": true, "when": true, "with_items": true, "loop": true,
	"loop_control": true, "register": true, "vars": true, "tags": true,
	"become": true, "become_user": true, "become_method": true,
	"notify": true, "block": true, "rescue": true, "always": true,
	"ignore_errors": true, "changed_when": true, "failed_when": true,
	"delegate_to": true, "run_once": true, "no_log": true,
	"environment": true, "args": true, "until": true, "retries": true,
	"delay": true, "check_mode": true, "diff": true, "listen": true,
	"any_errors_fatal": true, "throttle": true, "timeout": true,
	"connection": true, "remote_user": true, "vars_files": true,
	"with_dict": true, "with_fileglob": true, "with_first_found": true,
	"with_nested": true, "with_together": true, "with_subelements": true,
	"with_sequence": true, "with_random_choice": true, "with_lines": true,
	"with_flattened": true, "with_inventory_hostnames": true,
}

// extractAnsibleYAML detects and extracts an Ansible playbook or tasks /
// handlers file. Returns true when the file was recognised as Ansible
// (so the YAML extractor skips its generic top-level-keys fallback),
// false otherwise so the caller can run the generic path.
//
// Detection is conservative: a bare YAML list with no Ansible
// fingerprint (no `hosts:` play and no role-tree path) returns false so
// arbitrary config lists are not misclassified.
func extractAnsibleYAML(filePath, fileID string, src []byte, result *parser.ExtractionResult) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return false
	}
	seq := documentSequence(&doc)
	if seq == nil {
		return false
	}
	items := sequenceItems(seq)
	if len(items) == 0 {
		return false
	}

	pathHint := isAnsibleRolePath(filePath)
	playbookLike := isAnsiblePlaybook(items)
	tasksLike := pathHint && looksLikeTaskList(items)
	if !playbookLike && !tasksLike {
		return false
	}

	if playbookLike {
		for i, play := range items {
			if play.Kind != yaml.MappingNode {
				continue
			}
			extractAnsiblePlay(filePath, fileID, play, i, result)
		}
		return true
	}

	// Standalone tasks / handlers file: the file is the owner. A
	// `handlers/` path makes every item a handler; otherwise tasks.
	ansibleKind := "task"
	if isAnsibleHandlersPath(filePath) {
		ansibleKind = "handler"
	}
	for i, item := range items {
		if item.Kind != yaml.MappingNode {
			continue
		}
		extractAnsibleTask(filePath, fileID, item, i, ansibleKind, result)
	}
	return true
}

// isAnsiblePlaybook reports whether the sequence items look like plays:
// at least one item is a mapping carrying `hosts:` or an
// `import_playbook:` directive.
func isAnsiblePlaybook(items []*yaml.Node) bool {
	for _, item := range items {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if mappingGet(item, "hosts") != nil ||
			mappingGet(item, "import_playbook") != nil ||
			mappingGet(item, "ansible.builtin.import_playbook") != nil {
			return true
		}
	}
	return false
}

// looksLikeTaskList reports whether the sequence items look like tasks:
// every (mapping) item must carry exactly one module key (a key not in
// the task-directive set), so a plain list of config objects — whose
// keys are all "unknown" but which carries no `name:`/directive shape —
// does not slip through. We require that the majority of items name a
// module to keep the fingerprint tight.
func looksLikeTaskList(items []*yaml.Node) bool {
	mappings := 0
	withModule := 0
	for _, item := range items {
		if item.Kind != yaml.MappingNode {
			continue
		}
		mappings++
		if ansibleModuleOf(item) != "" || mappingGet(item, "block") != nil {
			withModule++
		}
	}
	return mappings > 0 && withModule == mappings
}

// extractAnsiblePlay emits the play node, its tasks, handlers, and role
// references.
func extractAnsiblePlay(filePath, fileID string, play *yaml.Node, index int, result *parser.ExtractionResult) {
	// An `import_playbook:` item is a play-level include, not a real
	// play body — record it as a reference and stop.
	if imp := scalarOf(mappingGet(play, "import_playbook")); imp != "" {
		emitPlaybookImport(filePath, fileID, imp, play.Line, result)
		return
	}
	if imp := scalarOf(mappingGet(play, "ansible.builtin.import_playbook")); imp != "" {
		emitPlaybookImport(filePath, fileID, imp, play.Line, result)
		return
	}

	hosts := scalarOf(mappingGet(play, "hosts"))
	playName := scalarOf(mappingGet(play, "name"))
	if playName == "" {
		playName = hosts
	}
	if playName == "" {
		playName = fmt.Sprintf("play-%d", index)
	}
	line := play.Line
	if line <= 0 {
		line = 1
	}

	playID := filePath + "::play:" + playName
	meta := map[string]any{"ansible_kind": "play"}
	if hosts != "" {
		meta["hosts"] = hosts
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: playID, Kind: graph.KindType, Name: playName,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "yaml", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: playID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})

	// roles: list of role names or {role: name} mappings.
	for _, role := range sequenceItems(mappingGet(play, "roles")) {
		roleName := ""
		switch role.Kind {
		case yaml.ScalarNode:
			roleName = role.Value
		case yaml.MappingNode:
			roleName = scalarOf(mappingGet(role, "role"))
			if roleName == "" {
				roleName = scalarOf(mappingGet(role, "name"))
			}
		}
		if roleName == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: playID, To: "unresolved::ansible_role::" + roleName,
			Kind: graph.EdgeReferences, FilePath: filePath, Line: role.Line,
			Meta: map[string]any{"ansible_ref": "role"},
		})
	}

	// tasks / pre_tasks / post_tasks: task lists owned by the play.
	for _, key := range []string{"pre_tasks", "tasks", "post_tasks"} {
		for i, task := range sequenceItems(mappingGet(play, key)) {
			if task.Kind != yaml.MappingNode {
				continue
			}
			extractAnsibleTaskGroup(filePath, playID, task, i, "task", result)
		}
	}

	// handlers: handler list owned by the play.
	for i, h := range sequenceItems(mappingGet(play, "handlers")) {
		if h.Kind != yaml.MappingNode {
			continue
		}
		extractAnsibleTaskGroup(filePath, playID, h, i, "handler", result)
	}
}

// emitPlaybookImport records an `import_playbook:` directive as an
// EdgeReferences from the file to the imported playbook.
func emitPlaybookImport(filePath, fileID, target string, line int, result *parser.ExtractionResult) {
	if line <= 0 {
		line = 1
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::ansible_playbook::" + target,
		Kind: graph.EdgeReferences, FilePath: filePath, Line: line,
		Meta: map[string]any{"ansible_ref": "import_playbook"},
	})
}

// extractAnsibleTaskGroup emits a task / handler node owned by ownerID.
// `block:` / `rescue:` / `always:` groups recurse into their sub-task
// lists instead of emitting a node for the group itself.
func extractAnsibleTaskGroup(filePath, ownerID string, task *yaml.Node, index int, ansibleKind string, result *parser.ExtractionResult) {
	if block := mappingGet(task, "block"); block != nil {
		for _, key := range []string{"block", "rescue", "always"} {
			for i, sub := range sequenceItems(mappingGet(task, key)) {
				if sub.Kind != yaml.MappingNode {
					continue
				}
				extractAnsibleTaskGroup(filePath, ownerID, sub, i, ansibleKind, result)
			}
		}
		return
	}
	emitAnsibleTaskNode(filePath, ownerID, task, index, ansibleKind, result)
}

// extractAnsibleTask is the standalone-file entry point: it owns the
// file node directly and handles block groups.
func extractAnsibleTask(filePath, fileID string, task *yaml.Node, index int, ansibleKind string, result *parser.ExtractionResult) {
	extractAnsibleTaskGroup(filePath, fileID, task, index, ansibleKind, result)
}

// emitAnsibleTaskNode appends one task / handler KindFunction node, its
// EdgeDefines from the owner, the module EdgeCalls, and any notify
// EdgeReferences.
func emitAnsibleTaskNode(filePath, ownerID string, task *yaml.Node, index int, ansibleKind string, result *parser.ExtractionResult) {
	module := ansibleModuleOf(task)
	taskName := scalarOf(mappingGet(task, "name"))
	if taskName == "" {
		if module != "" {
			taskName = fmt.Sprintf("%s-%d", ansibleLeafModule(module), index)
		} else {
			taskName = fmt.Sprintf("%s-%d", ansibleKind, index)
		}
	}
	line := task.Line
	if line <= 0 {
		line = 1
	}

	taskID := filePath + "::" + ansibleKind + ":" + taskName
	meta := map[string]any{"ansible_kind": ansibleKind}
	if module != "" {
		meta["module"] = module
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: taskID, Kind: graph.KindFunction, Name: taskName,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "yaml", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: ownerID, To: taskID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})

	if module != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: taskID, To: "unresolved::ansible_module::" + module,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	}

	// notify: a handler name or list of handler names.
	for _, h := range ansibleNotifyHandlers(mappingGet(task, "notify")) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: taskID, To: "unresolved::ansible_handler::" + h,
			Kind: graph.EdgeReferences, FilePath: filePath, Line: line,
			Meta: map[string]any{"ansible_ref": "notify"},
		})
	}
}

// ansibleModuleOf returns the module a task mapping invokes: the single
// key that is not a known task directive. Returns "" when the task
// names zero or more than one candidate module (ambiguous / not a task).
func ansibleModuleOf(task *yaml.Node) string {
	if task == nil || task.Kind != yaml.MappingNode {
		return ""
	}
	module := ""
	count := 0
	for i := 0; i+1 < len(task.Content); i += 2 {
		key := task.Content[i]
		if key == nil || key.Value == "" {
			continue
		}
		if ansibleTaskDirectives[key.Value] {
			continue
		}
		module = key.Value
		count++
	}
	if count == 1 {
		return module
	}
	return ""
}

// ansibleNotifyHandlers normalises a `notify:` value (scalar or
// sequence of scalars) into a list of handler names.
func ansibleNotifyHandlers(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value != "" {
			return []string{n.Value}
		}
	case yaml.SequenceNode:
		var out []string
		for _, c := range sequenceItems(n) {
			if c.Kind == yaml.ScalarNode && c.Value != "" {
				out = append(out, c.Value)
			}
		}
		return out
	}
	return nil
}

// ansibleLeafModule returns the short module name (the segment after the
// last `.`) so a fully-qualified `ansible.builtin.copy` yields `copy`
// for the synthesised task name.
func ansibleLeafModule(module string) string {
	if i := strings.LastIndex(module, "."); i >= 0 {
		return module[i+1:]
	}
	return module
}

// documentSequence returns the top-level SequenceNode for a parsed YAML
// document, unwrapping the DocumentNode. Returns nil when the document
// root is not a sequence (the K8s / dbt mapping-rooted sub-formats).
func documentSequence(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc == nil || doc.Kind != yaml.SequenceNode {
		return nil
	}
	return doc
}

// isAnsibleRolePath reports whether filePath sits in a conventional
// Ansible role tree (a `/tasks/`, `/handlers/`, or `/roles/` segment).
func isAnsibleRolePath(filePath string) bool {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	return strings.Contains(lower, "/tasks/") ||
		strings.Contains(lower, "/handlers/") ||
		strings.Contains(lower, "/roles/") ||
		strings.HasPrefix(lower, "tasks/") ||
		strings.HasPrefix(lower, "handlers/") ||
		strings.HasPrefix(lower, "roles/")
}

// isAnsibleHandlersPath reports whether filePath is a role handlers
// file (under a `handlers/` directory).
func isAnsibleHandlersPath(filePath string) bool {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	return strings.Contains(lower, "/handlers/") || strings.HasPrefix(lower, "handlers/")
}
