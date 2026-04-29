package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var statusIndex string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show index status: node/edge counts, languages, and file breakdown",
	RunE:  runStatus,
}

func init() {
	statusCmd.Flags().StringVar(&statusIndex, "index", ".", "repository path to index")
	rootCmd.AddCommand(statusCmd)
}

// runStatusViaDaemon prints status from the daemon. Returns nil on
// success, error to signal the caller to fall back to standalone
// indexing.
func runStatusViaDaemon(cmd *cobra.Command) error {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlStatus, nil)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("status rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	var st daemon.StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return fmt.Errorf("parse status: %w", err)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "daemon      %s (pid %d, uptime %s)\n",
		st.Version, st.PID, time.Duration(st.UptimeSeconds)*time.Second)
	_, _ = fmt.Fprintf(w, "sessions    %d\n", st.Sessions)
	if st.MemoryBytes > 0 {
		_, _ = fmt.Fprintf(w, "memory      %.1f MB\n", float64(st.MemoryBytes)/(1024*1024))
	}
	if len(st.TrackedRepos) == 0 {
		_, _ = fmt.Fprintln(w, "tracked repos: (none — run `gortex track <path>` to add one)")
		return nil
	}

	// One-line workspace rollup so the workspace boundary state is
	// visible in the compact view too. Mirrors renderDaemonWorkspaces'
	// compact
	// form: a single sentence when every repo is its own default
	// workspace, an explicit count when the user has grouped some.
	if len(st.Workspaces) > 0 {
		multiRepoWorkspaces := 0
		for _, ws := range st.Workspaces {
			if len(ws.Repos) > 1 {
				multiRepoWorkspaces++
			}
		}
		if multiRepoWorkspaces == 0 {
			_, _ = fmt.Fprintf(w, "workspaces  %d (one per repo, default)\n", len(st.Workspaces))
		} else {
			_, _ = fmt.Fprintf(w, "workspaces  %d (%d shared, %d default singletons)\n",
				len(st.Workspaces), multiRepoWorkspaces, len(st.Workspaces)-multiRepoWorkspaces)
		}
	}

	_, _ = fmt.Fprintln(w, "tracked repos:")
	// Sort by prefix for stable output across runs.
	sort.Slice(st.TrackedRepos, func(i, j int) bool {
		return st.TrackedRepos[i].Prefix < st.TrackedRepos[j].Prefix
	})
	// Workspace column only appears when the user actually has explicit
	// declarations — otherwise every row just repeats the repo prefix.
	showWS := false
	for _, r := range st.TrackedRepos {
		if r.Workspace != "" && r.Workspace != r.Prefix {
			showWS = true
			break
		}
	}
	for _, r := range st.TrackedRepos {
		if showWS {
			ws := r.Workspace
			if r.WorkspaceProject != "" && r.WorkspaceProject != ws {
				ws = ws + "/" + r.WorkspaceProject
			}
			_, _ = fmt.Fprintf(w, "  %-24s [%-12s] %s  (%d files, %d nodes, %d edges)\n",
				r.Prefix, ws, r.Path, r.Files, r.Nodes, r.Edges)
		} else {
			_, _ = fmt.Fprintf(w, "  %-24s %s  (%d files, %d nodes, %d edges)\n",
				r.Prefix, r.Path, r.Files, r.Nodes, r.Edges)
		}
	}
	return nil
}

func runStatus(cmd *cobra.Command, _ []string) error {
	// Daemon-first: if a daemon is running, query it for aggregate
	// status across all tracked repos. Falls back to the one-shot
	// local index for the standalone case.
	if daemon.IsRunning() {
		if err := runStatusViaDaemon(cmd); err == nil {
			return nil
		}
		// Any daemon error we didn't explicitly handle falls through to
		// local status — better to give the user something than nothing.
	}

	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)
	result, err := idx.Index(statusIndex)
	if err != nil {
		return fmt.Errorf("indexing failed: %w", err)
	}

	stats := g.Stats()

	_, _ = fmt.Fprintf(os.Stdout, "Repository:  %s\n", statusIndex)
	_, _ = fmt.Fprintf(os.Stdout, "Files:       %d\n", result.FileCount)
	_, _ = fmt.Fprintf(os.Stdout, "Nodes:       %d\n", stats.TotalNodes)
	_, _ = fmt.Fprintf(os.Stdout, "Edges:       %d\n", stats.TotalEdges)
	_, _ = fmt.Fprintf(os.Stdout, "Duration:    %dms\n\n", result.DurationMs)

	if len(stats.ByLanguage) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Languages:")
		for lang, count := range stats.ByLanguage {
			_, _ = fmt.Fprintf(os.Stdout, "  %-14s %d nodes\n", lang, count)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}

	if len(stats.ByKind) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "By kind:")
		for kind, count := range stats.ByKind {
			_, _ = fmt.Fprintf(os.Stdout, "  %-14s %d\n", kind, count)
		}
	}

	// Display per-repo and per-project stats from GlobalConfig.
	gc, err := config.LoadGlobal()
	if err == nil {
		printMultiRepoStatus(gc, g)
	}

	return nil
}

// printMultiRepoStatus displays per-repo and per-project statistics from the GlobalConfig.
func printMultiRepoStatus(gc *config.GlobalConfig, g *graph.Graph) {
	repoStats := g.RepoStats()
	hasMultiRepo := len(repoStats) > 1 || len(gc.Repos) > 0 || len(gc.Projects) > 0

	if !hasMultiRepo {
		return
	}

	_, _ = fmt.Fprintln(os.Stdout)

	// Active project indicator.
	if gc.ActiveProject != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Active project: %s\n\n", gc.ActiveProject)
	}

	// Per-repo stats.
	if len(gc.Repos) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Tracked repositories:")

		// Build a set of repos shared across projects.
		sharedRepos := findSharedRepos(gc)

		for _, repo := range gc.Repos {
			prefix := config.ResolvePrefix(repo)
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", prefix)
			_, _ = fmt.Fprintf(os.Stdout, "    Path: %s\n", repo.Path)

			if repo.Ref != "" {
				_, _ = fmt.Fprintf(os.Stdout, "    Ref:  %s\n", repo.Ref)
			}

			// Show graph stats if available.
			if rs, ok := repoStats[prefix]; ok {
				_, _ = fmt.Fprintf(os.Stdout, "    Nodes: %d  Edges: %d\n", rs.TotalNodes, rs.TotalEdges)
			}

			// Indicate shared repos.
			if projects, ok := sharedRepos[repo.Path]; ok && len(projects) > 0 {
				_, _ = fmt.Fprintf(os.Stdout, "    Shared in: %s\n", strings.Join(projects, ", "))
			}
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}

	// Per-project stats.
	if len(gc.Projects) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Projects:")

		// Sort project names for deterministic output.
		projNames := make([]string, 0, len(gc.Projects))
		for name := range gc.Projects {
			projNames = append(projNames, name)
		}
		sort.Strings(projNames)

		for _, projName := range projNames {
			proj := gc.Projects[projName]
			active := ""
			if projName == gc.ActiveProject {
				active = " (active)"
			}
			_, _ = fmt.Fprintf(os.Stdout, "  %s%s\n", projName, active)

			// Aggregate counts for the project.
			var totalNodes, totalEdges int
			for _, repo := range proj.Repos {
				prefix := config.ResolvePrefix(repo)
				refTag := ""
				if repo.Ref != "" {
					refTag = fmt.Sprintf(" [%s]", repo.Ref)
				}
				_, _ = fmt.Fprintf(os.Stdout, "    - %s%s (%s)\n", prefix, refTag, repo.Path)

				if rs, ok := repoStats[prefix]; ok {
					totalNodes += rs.TotalNodes
					totalEdges += rs.TotalEdges
				}
			}
			_, _ = fmt.Fprintf(os.Stdout, "    Total: %d nodes, %d edges\n", totalNodes, totalEdges)
		}
	}
}

// findSharedRepos returns a map of repo path → list of project names that include it.
// Only repos appearing in 2+ projects are included.
func findSharedRepos(gc *config.GlobalConfig) map[string][]string {
	pathProjects := make(map[string][]string)
	for projName, proj := range gc.Projects {
		for _, repo := range proj.Repos {
			pathProjects[repo.Path] = append(pathProjects[repo.Path], projName)
		}
	}

	// Filter to only shared repos.
	shared := make(map[string][]string)
	for path, projects := range pathProjects {
		if len(projects) > 1 {
			sort.Strings(projects)
			shared[path] = projects
		}
	}
	return shared
}
