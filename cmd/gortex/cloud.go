package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// cloud is the umbrella command for all cloud-side flows that run
// from the local CLI: login, list, logout. These commands talk to
// the gortex-cloud control plane (API) — they never run cloud code
// in-process. The two repos are decoupled at the module boundary.
var cloudCmd = &cobra.Command{
	Use:   "cloud",
	Short: "Manage Gortex Cloud connections (login, list, logout)",
}

var (
	cloudEndpoint  string
	cloudWorkspace string
	cloudToken     string
)

var cloudLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Connect this machine to a Gortex Cloud workspace",
	Long: `Add a [[server]] entry to ~/.gortex/servers.toml pointing at the
gortex-cloud control plane (mcp.gortex.dev/v1 by default) for one
workspace. The token is the gtx_live_… string the admin console
prints once at issuance.

Example:
  gortex cloud login --endpoint https://mcp.gortex.dev/v1 \
      --workspace tuck --token gtx_live_abc...
`,
	RunE: runCloudLogin,
}

var cloudListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show every cloud workspace this machine is connected to",
	RunE:  runCloudList,
}

var cloudLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove a cloud workspace from ~/.gortex/servers.toml",
	RunE:  runCloudLogout,
}

func init() {
	cloudLoginCmd.Flags().StringVar(&cloudEndpoint, "endpoint", "https://mcp.gortex.dev/v1", "cloud control-plane base URL")
	cloudLoginCmd.Flags().StringVar(&cloudWorkspace, "workspace", "", "workspace slug to bind this token to (required)")
	cloudLoginCmd.Flags().StringVar(&cloudToken, "token", "", "API token (gtx_live_… or gtx_test_…) issued in the admin console (required)")
	_ = cloudLoginCmd.MarkFlagRequired("workspace")
	_ = cloudLoginCmd.MarkFlagRequired("token")

	cloudLogoutCmd.Flags().StringVar(&cloudWorkspace, "workspace", "", "workspace slug to disconnect (required)")
	_ = cloudLogoutCmd.MarkFlagRequired("workspace")

	cloudCmd.AddCommand(cloudLoginCmd, cloudListCmd, cloudLogoutCmd)
	rootCmd.AddCommand(cloudCmd)
}

func runCloudLogin(_ *cobra.Command, _ []string) error {
	cloudToken = strings.TrimSpace(cloudToken)
	if cloudToken == "" {
		return errors.New("--token is required")
	}
	if !strings.HasPrefix(cloudToken, "gtx_live_") && !strings.HasPrefix(cloudToken, "gtx_test_") {
		return errors.New("--token must start with gtx_live_ or gtx_test_")
	}
	if cloudWorkspace == "" {
		return errors.New("--workspace is required")
	}

	// Verify the token by hitting /v1/workspaces/repos against the
	// endpoint. If the call returns 401 we surface it before
	// persisting any config.
	if err := pingCloudEndpoint(cloudEndpoint, cloudToken); err != nil {
		return fmt.Errorf("verify cloud connection: %w", err)
	}

	path := daemon.ServersConfigPath()
	cfg, err := daemon.LoadServersConfig(path)
	if err != nil {
		return fmt.Errorf("load servers config: %w", err)
	}

	slug := "cloud-" + cloudWorkspace
	for i, s := range cfg.Server {
		if s.Slug == slug {
			cfg.Server = append(cfg.Server[:i], cfg.Server[i+1:]...)
			break
		}
	}
	if err := cfg.AddServer(daemon.ServerEntry{
		Slug:       slug,
		URL:        cloudEndpoint,
		AuthToken:  cloudToken,
		Workspaces: []string{cloudWorkspace},
	}); err != nil {
		return fmt.Errorf("add server: %w", err)
	}
	if err := cfg.Save(path); err != nil {
		return fmt.Errorf("save servers config: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "logged in: workspace=%s endpoint=%s slug=%s\n", cloudWorkspace, cloudEndpoint, slug)
	_, _ = fmt.Fprintf(os.Stdout, "next: gortex daemon start (or already running)\n")
	return nil
}

func runCloudList(_ *cobra.Command, _ []string) error {
	cfg, err := daemon.LoadServersConfig(daemon.ServersConfigPath())
	if err != nil {
		return err
	}
	if len(cfg.Server) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no [[server]] entries — run `gortex cloud login` to add one")
		return nil
	}
	_, _ = fmt.Fprintf(os.Stdout, "%-24s %-46s %s\n", "SLUG", "ENDPOINT", "WORKSPACES")
	for _, s := range cfg.Server {
		_, _ = fmt.Fprintf(os.Stdout, "%-24s %-46s %s\n", s.Slug, s.URL, strings.Join(s.Workspaces, ","))
	}
	return nil
}

func runCloudLogout(_ *cobra.Command, _ []string) error {
	path := daemon.ServersConfigPath()
	cfg, err := daemon.LoadServersConfig(path)
	if err != nil {
		return err
	}
	slug := "cloud-" + cloudWorkspace
	out := cfg.Server[:0]
	removed := false
	for _, s := range cfg.Server {
		if s.Slug == slug {
			removed = true
			continue
		}
		out = append(out, s)
	}
	cfg.Server = out
	if !removed {
		return fmt.Errorf("no entry with slug %q found", slug)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "logged out: %s\n", slug)
	return nil
}

func pingCloudEndpoint(endpoint, token string) error {
	if !strings.HasSuffix(endpoint, "/") {
		endpoint += "/"
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimSuffix(endpoint, "/")+"/workspaces/repos", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("token rejected (401)")
	}
	// Show a short tail of the response to help debug.
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Errorf("unexpected %d: %s", resp.StatusCode, bytes.TrimSpace(body))
}

// Keep encoding/json import — `gortex cloud login` will grow to
// parse a /v1/me response in a follow-up; declaring it now keeps
// the import section stable for that change.
var _ = json.RawMessage(nil)
