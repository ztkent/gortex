package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/githooks"
)

var (
	githookRegenMermaid  bool
	githookRegenWiki     bool
	githookRegenDocs     bool
	githookMermaidOutDir string
	githookWikiOutDir    string
	githookDocsOutPath   string
	githookBinary        string
)

var githookCmd = &cobra.Command{
	Use:   "githook",
	Short: "Manage local git hooks that regenerate gortex artefacts",
	Long: `Install, uninstall, and inspect the post-commit hook that re-runs
gortex commands after each commit.

The hook is idempotent: re-running install replaces only the gortex
block, leaving any other hook content intact. Uninstall removes the
block and deletes the hook file when it contains nothing else.`,
}

var githookInstallCmd = &cobra.Command{
	Use:   "install <hook>",
	Short: "Install a git hook (currently: post-commit)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGithookInstall,
}

var githookUninstallCmd = &cobra.Command{
	Use:   "uninstall <hook>",
	Short: "Remove the gortex-managed block from a git hook",
	Args:  cobra.ExactArgs(1),
	RunE:  runGithookUninstall,
}

var githookStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the post-commit hook is gortex-managed",
	RunE:  runGithookStatus,
}

func init() {
	githookInstallCmd.Flags().BoolVar(&githookRegenMermaid, "regen-mermaid", false,
		"include `gortex export --format mermaid --scope all` in the hook")
	githookInstallCmd.Flags().BoolVar(&githookRegenWiki, "regen-wiki", false,
		"include `gortex wiki .` in the hook")
	githookInstallCmd.Flags().BoolVar(&githookRegenDocs, "regen-docs", false,
		"include `gortex docs . --out CHANGELOG_AUTO.md` in the hook")
	githookInstallCmd.Flags().StringVar(&githookMermaidOutDir, "mermaid-out-dir", "docs/architecture/",
		"output directory for mermaid diagrams")
	githookInstallCmd.Flags().StringVar(&githookWikiOutDir, "wiki-out-dir", "wiki",
		"output directory for the wiki")
	githookInstallCmd.Flags().StringVar(&githookDocsOutPath, "docs-out-path", "CHANGELOG_AUTO.md",
		"output path for the docs bundle")
	githookInstallCmd.Flags().StringVar(&githookBinary, "binary", "gortex",
		"gortex binary name (resolved from $PATH at runtime)")

	githookCmd.AddCommand(githookInstallCmd)
	githookCmd.AddCommand(githookUninstallCmd)
	githookCmd.AddCommand(githookStatusCmd)
	rootCmd.AddCommand(githookCmd)
}

func runGithookInstall(cmd *cobra.Command, args []string) error {
	hook := args[0]
	if hook != "post-commit" {
		return fmt.Errorf("only the post-commit hook is supported (got %q)", hook)
	}
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	if !githookRegenMermaid && !githookRegenWiki && !githookRegenDocs {
		// Default to mermaid when nothing was chosen — minimum
		// useful behaviour.
		githookRegenMermaid = true
	}
	path, err := githooks.InstallPostCommit(repoRoot, githooks.InstallOpts{
		Binary:        githookBinary,
		RegenMermaid:  githookRegenMermaid,
		RegenWiki:     githookRegenWiki,
		RegenDocs:     githookRegenDocs,
		MermaidOutDir: githookMermaidOutDir,
		WikiOutDir:    githookWikiOutDir,
		DocsOutPath:   githookDocsOutPath,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"installed post-commit hook at %s\nactions: mermaid=%t wiki=%t docs=%t\n",
		path, githookRegenMermaid, githookRegenWiki, githookRegenDocs)
	return nil
}

func runGithookUninstall(cmd *cobra.Command, args []string) error {
	hook := args[0]
	if hook != "post-commit" {
		return fmt.Errorf("only the post-commit hook is supported (got %q)", hook)
	}
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	path, removed, err := githooks.UninstallPostCommit(repoRoot)
	if err != nil {
		return err
	}
	if removed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed gortex block from %s\n", path)
	} else {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no gortex block found at %s\n", path)
	}
	return nil
}

func runGithookStatus(cmd *cobra.Command, _ []string) error {
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	rep, err := githooks.Status(repoRoot)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "hook_path: %s\n", rep.HookPath)
	_, _ = fmt.Fprintf(out, "exists:    %t\n", rep.Exists)
	_, _ = fmt.Fprintf(out, "managed:   %t\n", rep.Managed)
	return nil
}

// resolveGithookRepoRoot picks the repo root for the active command.
// Today we always use the working directory; the install runs git
// itself to resolve git-dir and core.hooksPath.
func resolveGithookRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(cwd), nil
}
