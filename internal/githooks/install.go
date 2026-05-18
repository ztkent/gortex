// Package githooks installs local git hooks that re-run gortex
// commands after specified events. The implementation is read-only on
// git itself — it shells out only for `git rev-parse` and
// `git config --get core.hooksPath`. Hook files are managed by
// markers so we can install and uninstall idempotently without
// destroying any user-authored content.
package githooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Begin and end markers wrap the gortex-managed block inside a hook
// file. The MARKER_BEGIN / MARKER_END convention is checked by every
// install/uninstall pass and never re-written verbatim by the user.
const (
	MarkerBegin = "# gortex-managed:post-commit:begin"
	MarkerEnd   = "# gortex-managed:post-commit:end"
)

// InstallOpts controls what the installed hook runs.
type InstallOpts struct {
	// Binary is the gortex executable path. Defaults to "gortex"
	// (found via $PATH at runtime).
	Binary string
	// RegenMermaid toggles `gortex export --format mermaid --scope all`.
	RegenMermaid bool
	// MermaidOutDir is where the mermaid exporter writes its files.
	// Defaults to "docs/architecture/".
	MermaidOutDir string
	// RegenWiki toggles a `gortex wiki .` run.
	RegenWiki bool
	// WikiOutDir is where the wiki is written. Defaults to "wiki".
	WikiOutDir string
	// RegenDocs toggles a `gortex docs . --out CHANGELOG_AUTO.md` run.
	RegenDocs bool
	// DocsOutPath is the docs bundle output path. Defaults to
	// "CHANGELOG_AUTO.md".
	DocsOutPath string
}

func (o InstallOpts) withDefaults() InstallOpts {
	if strings.TrimSpace(o.Binary) == "" {
		o.Binary = "gortex"
	}
	if o.MermaidOutDir == "" {
		o.MermaidOutDir = "docs/architecture/"
	}
	if o.WikiOutDir == "" {
		o.WikiOutDir = "wiki"
	}
	if o.DocsOutPath == "" {
		o.DocsOutPath = "CHANGELOG_AUTO.md"
	}
	return o
}

// hookCommands builds the body the installer writes inside the
// marker block. The body is a `#!/bin/sh` snippet that runs every
// enabled action and tolerates failures so the commit still
// completes when gortex isn't on PATH.
func hookCommands(opts InstallOpts) []string {
	var cmds []string
	cmds = append(cmds, "# Auto-regenerate gortex artefacts after each commit.")
	cmds = append(cmds, "# Failures are tolerated so the commit always completes.")
	if opts.RegenMermaid {
		cmds = append(cmds, fmt.Sprintf("(%s export --format mermaid --scope all --out-dir %q --on-commit) >/dev/null 2>&1 || true",
			opts.Binary, opts.MermaidOutDir))
	}
	if opts.RegenWiki {
		cmds = append(cmds, fmt.Sprintf("(%s wiki . --output %q) >/dev/null 2>&1 || true",
			opts.Binary, opts.WikiOutDir))
	}
	if opts.RegenDocs {
		cmds = append(cmds, fmt.Sprintf("(%s docs . --out %q) >/dev/null 2>&1 || true",
			opts.Binary, opts.DocsOutPath))
	}
	if len(cmds) == 2 {
		// No actions selected — note it explicitly.
		cmds = append(cmds, "# (no regeneration actions enabled)")
	}
	return cmds
}

// HookPath resolves the absolute path of the post-commit hook for the
// repository rooted at repoRoot. Honours core.hooksPath when set.
func HookPath(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("githooks: repoRoot is empty")
	}
	gitDir, err := runGit(repoRoot, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("githooks: not a git repository at %q: %w", repoRoot, err)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	customPath, _ := runGit(repoRoot, "config", "--get", "core.hooksPath")
	hooksDir := filepath.Join(gitDir, "hooks")
	if cp := strings.TrimSpace(customPath); cp != "" {
		if !filepath.IsAbs(cp) {
			cp = filepath.Join(repoRoot, cp)
		}
		hooksDir = cp
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", fmt.Errorf("githooks: create hooks dir %q: %w", hooksDir, err)
	}
	return filepath.Join(hooksDir, "post-commit"), nil
}

// StatusReport describes the current state of the post-commit hook.
type StatusReport struct {
	HookPath  string `json:"hook_path"`
	Exists    bool   `json:"exists"`
	Managed   bool   `json:"managed"` // true iff our marker block is present
	Body      string `json:"body,omitempty"`
}

// Status reports the current state of the post-commit hook. Never
// modifies anything.
func Status(repoRoot string) (StatusReport, error) {
	path, err := HookPath(repoRoot)
	if err != nil {
		return StatusReport{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusReport{HookPath: path}, nil
		}
		return StatusReport{}, fmt.Errorf("githooks: read %q: %w", path, err)
	}
	rep := StatusReport{
		HookPath: path,
		Exists:   true,
		Body:     string(body),
	}
	if bytes.Contains(body, []byte(MarkerBegin)) && bytes.Contains(body, []byte(MarkerEnd)) {
		rep.Managed = true
	}
	return rep, nil
}

// InstallPostCommit writes a post-commit hook with the configured
// commands inside our marker block. Idempotent: re-running replaces
// just the gortex block, leaving any other content intact.
//
// Returns the absolute path of the hook so callers can show it to the
// user.
func InstallPostCommit(repoRoot string, opts InstallOpts) (string, error) {
	opts = opts.withDefaults()
	hookPath, err := HookPath(repoRoot)
	if err != nil {
		return "", err
	}

	cmds := hookCommands(opts)

	var newBlock bytes.Buffer
	newBlock.WriteString(MarkerBegin)
	newBlock.WriteString("\n")
	for _, line := range cmds {
		newBlock.WriteString(line)
		newBlock.WriteString("\n")
	}
	newBlock.WriteString(MarkerEnd)
	newBlock.WriteString("\n")

	existing, _ := os.ReadFile(hookPath) // nil bytes when file doesn't exist
	var out bytes.Buffer
	if len(existing) == 0 {
		out.WriteString("#!/bin/sh\n")
		out.WriteString("# Installed by `gortex githook install post-commit`.\n")
		out.WriteString("# Marker block below is regenerated on each install/uninstall;\n")
		out.WriteString("# add your own commands outside the markers and they will be preserved.\n\n")
		out.Write(newBlock.Bytes())
	} else {
		body := string(existing)
		// Ensure the shebang is present so the hook is executable.
		if !strings.HasPrefix(body, "#!") {
			out.WriteString("#!/bin/sh\n")
		}
		if strings.Contains(body, MarkerBegin) && strings.Contains(body, MarkerEnd) {
			// Replace existing block.
			before, rest, _ := strings.Cut(body, MarkerBegin)
			_, after, _ := strings.Cut(rest, MarkerEnd)
			after = strings.TrimLeft(after, "\n")
			out.WriteString(before)
			out.Write(newBlock.Bytes())
			out.WriteString(after)
		} else {
			// Append a new block.
			out.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				out.WriteString("\n")
			}
			out.WriteString("\n")
			out.Write(newBlock.Bytes())
		}
	}

	if err := os.WriteFile(hookPath, out.Bytes(), 0o755); err != nil {
		return "", fmt.Errorf("githooks: write %q: %w", hookPath, err)
	}
	// Make sure the bit is set even if the file already existed.
	_ = os.Chmod(hookPath, 0o755)
	return hookPath, nil
}

// UninstallPostCommit removes the gortex-managed block. If the file
// then contains nothing but the shebang and our installer comment,
// the file is deleted entirely. Otherwise we leave the residual
// (user-authored) content in place.
//
// Returns the path of the hook (whether it now exists or was deleted)
// and a bool indicating "block was found and removed".
func UninstallPostCommit(repoRoot string) (string, bool, error) {
	hookPath, err := HookPath(repoRoot)
	if err != nil {
		return "", false, err
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return hookPath, false, nil
		}
		return "", false, err
	}
	b := string(body)
	if !strings.Contains(b, MarkerBegin) || !strings.Contains(b, MarkerEnd) {
		return hookPath, false, nil
	}
	before, rest, _ := strings.Cut(b, MarkerBegin)
	_, after, _ := strings.Cut(rest, MarkerEnd)
	after = strings.TrimLeft(after, "\n")
	cleaned := strings.TrimRight(before, "\n") + "\n" + after
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || isInstallerStub(cleaned) {
		// Only the shebang + comments left — drop the file.
		if err := os.Remove(hookPath); err != nil {
			return "", false, err
		}
		return hookPath, true, nil
	}
	if !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	if err := os.WriteFile(hookPath, []byte(cleaned), 0o755); err != nil {
		return "", false, fmt.Errorf("githooks: write %q: %w", hookPath, err)
	}
	return hookPath, true, nil
}

// isInstallerStub returns true when the residual content is just the
// shebang and the installer-comment header we inserted on first
// install — i.e. nothing the user added themselves.
func isInstallerStub(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "#") {
			continue
		}
		// Found a non-comment, non-blank line — keep the file.
		return false
	}
	return true
}

// runGit invokes `git` from inside repoRoot and returns trimmed
// stdout. Errors carry stderr context.
func runGit(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
