package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Inspect and edit per-repo workspace declarations",
	Long: `Manage the per-repo .gortex.yaml workspace and project slugs.

The hard graph boundary introduced by spec-launch.md §4 keys every
node and contract on its workspace. Two repos that should pair their
contracts as one logical service must declare the same 'workspace:'
in their respective .gortex.yaml files. Without that declaration,
each repo defaults to its own workspace (named after the repo) and
cross-repo contract matching stops at the boundary.

This command is the migration path: it lists what each tracked repo
currently declares and writes new declarations atomically without
disturbing other keys in the file.`,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the workspace and project declared in each tracked repo's .gortex.yaml",
	RunE:  runWorkspaceList,
}

var workspaceSetCmd = &cobra.Command{
	Use:   "set <repo> <workspace> [project]",
	Short: "Write workspace (and optional project) for a tracked repo",
	Long: `Updates one repo's workspace declaration.

The repo argument is matched against the global config: it can be
a name (e.g. 'tuck-api'), an absolute path, or a path relative to
the cwd. Project defaults to the workspace slug when omitted.

Without --global the value is written to the repo's .gortex.yaml
(the spec source of truth). With --global the value is written to
~/.config/gortex/config.yaml (your user-level config), which is
the right choice for OSS / read-only repos where you don't want
to leave any artifact in the repo. Global overrides win over
.gortex.yaml at resolution time, so you can also use --global to
override a workspace the repo author chose.`,
	Args: cobra.RangeArgs(2, 3),
	RunE: runWorkspaceSet,
}

var (
	workspaceSetAll    bool
	workspaceSetRoot   string
	workspaceSetGlobal bool
)

var workspaceSetAllCmd = &cobra.Command{
	Use:   "set-all <workspace>",
	Short: "Stamp the same workspace into every tracked repo (interactive confirmation)",
	Long: `Bulk migration helper. Walks every tracked repo in the global config
and writes the same 'workspace:' value to each.

By default the command prints the planned changes and asks for
confirmation. Pass --yes to skip the prompt (CI / scripted use).
--root <path> restricts the bulk update to repos under that prefix
(e.g. only your "work" repos). --global writes to
~/.config/gortex/config.yaml instead of touching each repo's
.gortex.yaml — the OSS-friendly path.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceSetAll,
}

func init() {
	workspaceCmd.AddCommand(workspaceListCmd)
	workspaceCmd.AddCommand(workspaceSetCmd)
	workspaceCmd.AddCommand(workspaceSetAllCmd)
	workspaceSetCmd.Flags().BoolVar(&workspaceSetGlobal, "global", false, "write to ~/.config/gortex/config.yaml instead of the repo's .gortex.yaml (OSS-friendly)")
	workspaceSetAllCmd.Flags().BoolVarP(&workspaceSetAll, "yes", "y", false, "skip interactive confirmation")
	workspaceSetAllCmd.Flags().StringVar(&workspaceSetRoot, "root", "", "only stamp repos whose path starts with this prefix")
	workspaceSetAllCmd.Flags().BoolVar(&workspaceSetGlobal, "global", false, "write to ~/.config/gortex/config.yaml instead of each repo's .gortex.yaml")
	rootCmd.AddCommand(workspaceCmd)
}

// loadGlobalRepos reads the global config (~/.config/gortex/config.yaml
// by default, or whatever --config points at) and returns the tracked
// repo entries. Failure to read the config returns an error rather
// than an empty list — silently doing nothing on a typo'd config
// path is a worse default than failing loudly.
func loadGlobalRepos() ([]config.RepoEntry, error) {
	path := config.DefaultGlobalConfigPath()
	if cfgFile != "" {
		path = cfgFile
	}
	gc, err := config.LoadGlobal(path)
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	return gc.Repos, nil
}

// readRepoYAML loads <repoPath>/.gortex.yaml and returns the parsed
// generic map. Missing file is not an error — returns an empty map
// so callers can write a fresh file.
func readRepoYAML(repoPath string) (map[string]any, error) {
	p := filepath.Join(repoPath, ".gortex.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	out := make(map[string]any)
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if out == nil {
		out = make(map[string]any)
	}
	return out, nil
}

// writeRepoYAML serialises the map back to <repoPath>/.gortex.yaml.
// Uses an atomic tmp+rename so a partial write doesn't leave the
// file truncated. The yaml.v3 encoder preserves key order from the
// input map's iteration which is non-deterministic; we sort keys to
// keep diffs readable.
func writeRepoYAML(repoPath string, payload map[string]any) error {
	p := filepath.Join(repoPath, ".gortex.yaml")
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Hand-roll the encoder so we can write keys in deterministic
	// order. yaml.v3 doesn't expose a sorted-encode helper.
	var buf strings.Builder
	for _, k := range keys {
		v, err := yaml.Marshal(map[string]any{k: payload[k]})
		if err != nil {
			return fmt.Errorf("marshal %s: %w", k, err)
		}
		buf.Write(v)
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, p, err)
	}
	return nil
}

// matchRepo finds the RepoEntry whose name OR absolute path matches
// `target`. Returns (-1, error) when no entry matches.
func matchRepo(repos []config.RepoEntry, target string) (int, error) {
	abs, _ := filepath.Abs(target)
	for i, r := range repos {
		if r.Name == target {
			return i, nil
		}
		ra, _ := filepath.Abs(r.Path)
		if ra == abs {
			return i, nil
		}
		// Allow short suffix matches like "tuck-api" against
		// "/Users/x/code/work/tuck-api".
		if strings.HasSuffix(r.Path, "/"+target) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no tracked repo matches %q (try `gortex daemon status` for the list)", target)
}

func runWorkspaceList(cmd *cobra.Command, _ []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos)")
		return nil
	}

	t := table.NewWriter()
	t.SetOutputMirror(cmd.OutOrStdout())
	t.SetStyle(table.StyleLight)
	t.AppendHeader(table.Row{"repo", "workspace", "project", "source", "path"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignLeft},
		{Number: 5, Align: text.AlignLeft},
	})

	for _, r := range repos {
		// Precedence: global config (RepoEntry) > .gortex.yaml > default.
		ws, proj, src := "", "", ""
		if r.Workspace != "" || r.Project != "" {
			ws, proj, src = r.Workspace, r.Project, "global"
		}
		yamlWS, yamlProj := "", ""
		payload, yamlErr := readRepoYAML(r.Path)
		if yamlErr == nil {
			yamlWS, _ = payload["workspace"].(string)
			yamlProj, _ = payload["project"].(string)
			if ws == "" {
				ws = yamlWS
				if ws != "" {
					src = ".gortex.yaml"
				}
			}
			if proj == "" {
				proj = yamlProj
			}
		} else {
			t.AppendRow(table.Row{repoLabel(r), "(error)", yamlErr.Error(), "", r.Path})
			continue
		}
		// Note when global override is shadowing a repo declaration.
		if r.Workspace != "" && yamlWS != "" && r.Workspace != yamlWS {
			src = "global (overrides .gortex.yaml=" + yamlWS + ")"
		}
		if ws == "" {
			ws = "(default: " + repoLabel(r) + ")"
			if src == "" {
				src = "default"
			}
		}
		if proj == "" {
			proj = "(default: " + repoLabel(r) + ")"
		}
		t.AppendRow(table.Row{repoLabel(r), ws, proj, src, r.Path})
	}
	t.Render()
	return nil
}

func repoLabel(r config.RepoEntry) string {
	if r.Name != "" {
		return r.Name
	}
	return filepath.Base(r.Path)
}

func runWorkspaceSet(cmd *cobra.Command, args []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}
	idx, err := matchRepo(repos, args[0])
	if err != nil {
		return err
	}
	workspace := args[1]
	project := workspace
	if len(args) >= 3 {
		project = args[2]
	}
	r := repos[idx]

	if workspaceSetGlobal {
		path, err := stampWorkspaceGlobal(r, workspace, project)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"updated %s: %s → workspace=%s project=%s\n",
			path, repoLabel(r), workspace, project)
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"\nNote: a running daemon needs `gortex daemon reload` (or restart) to pick up the change.")
		return nil
	}

	if err := stampWorkspace(r.Path, workspace, project); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"updated %s/.gortex.yaml: workspace=%s project=%s\n",
		r.Path, workspace, project)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(),
		"\nNote: a running daemon needs `gortex daemon reload` (or restart) to pick up the change.")
	return nil
}

// stampWorkspace writes workspace+project into a repo's .gortex.yaml
// without disturbing other keys (guards, exclude rules, semantic
// providers, etc.). Idempotent — re-running with the same values
// is a no-op rewrite.
func stampWorkspace(repoPath, workspace, project string) error {
	payload, err := readRepoYAML(repoPath)
	if err != nil {
		return err
	}
	payload["workspace"] = workspace
	payload["project"] = project
	return writeRepoYAML(repoPath, payload)
}

// stampWorkspaceGlobal writes the workspace/project override onto
// the matching RepoEntry in `~/.config/gortex/config.yaml`. Returns
// the path of the file modified for the user-facing message. Used
// when the user passes --global — the OSS-friendly path that
// leaves no trace in the repo itself.
func stampWorkspaceGlobal(target config.RepoEntry, workspace, project string) (string, error) {
	path := config.DefaultGlobalConfigPath()
	if cfgFile != "" {
		path = cfgFile
	}
	gc, err := config.LoadGlobal(path)
	if err != nil {
		return "", fmt.Errorf("load global config: %w", err)
	}
	updated := false
	for i := range gc.Repos {
		// Match on absolute path — the most stable identity. Name and
		// prefix can collide (two repos can share a basename) but a
		// path is unique to the on-disk location.
		ra, _ := filepath.Abs(gc.Repos[i].Path)
		ta, _ := filepath.Abs(target.Path)
		if ra != ta {
			continue
		}
		gc.Repos[i].Workspace = workspace
		gc.Repos[i].Project = project
		updated = true
		break
	}
	if !updated {
		return "", fmt.Errorf("repo %s not found in %s", target.Path, path)
	}
	if err := gc.Save(); err != nil {
		return "", fmt.Errorf("save global config: %w", err)
	}
	return path, nil
}

func runWorkspaceSetAll(cmd *cobra.Command, args []string) error {
	workspace := args[0]
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos to update)")
		return nil
	}

	// Filter to --root prefix when set. Match on absolute path
	// prefix so 'gortex workspace set-all work --root ~/code/work'
	// only stamps repos under the work tree.
	var rootAbs string
	if workspaceSetRoot != "" {
		rootAbs, err = filepath.Abs(workspaceSetRoot)
		if err != nil {
			return fmt.Errorf("resolve --root: %w", err)
		}
	}
	planned := make([]config.RepoEntry, 0, len(repos))
	for _, r := range repos {
		if rootAbs != "" {
			ra, _ := filepath.Abs(r.Path)
			if !strings.HasPrefix(ra, rootAbs) {
				continue
			}
		}
		planned = append(planned, r)
	}
	if len(planned) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos match --root)")
		return nil
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Plan: stamp workspace=%q (project=workspace) into %d repos:\n", workspace, len(planned))
	for _, r := range planned {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", r.Path)
	}
	if !workspaceSetAll {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "\nProceed? [y/N] ")
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return nil
		}
	}

	updated := 0
	var failures []string
	for _, r := range planned {
		var stampErr error
		if workspaceSetGlobal {
			_, stampErr = stampWorkspaceGlobal(r, workspace, workspace)
		} else {
			stampErr = stampWorkspace(r.Path, workspace, workspace)
		}
		if stampErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.Path, stampErr))
			continue
		}
		updated++
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nupdated %d/%d repos\n", updated, len(planned))
	if len(failures) > 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nfailures:")
		for _, f := range failures {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", f)
		}
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nNote: a running daemon needs `gortex daemon reload` (or restart) to pick up the change.")
	return nil
}
