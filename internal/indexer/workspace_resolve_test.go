package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// Precedence: RepoEntry.Workspace (global config override) wins over
// .gortex.yaml::workspace, which wins over the repo-prefix default.
// Mirrors the resolution chain in resolveWorkspaceID's docstring.
func TestResolveWorkspaceID_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		entry    *config.RepoEntry
		cfg      *config.Config
		prefix   string
		want     string
	}{
		{
			name:   "all empty falls back to prefix",
			prefix: "myrepo",
			want:   "myrepo",
		},
		{
			name:   ".gortex.yaml wins over default",
			cfg:    &config.Config{Workspace: "tuck"},
			prefix: "tuck-api",
			want:   "tuck",
		},
		{
			name:   "RepoEntry.Workspace overrides .gortex.yaml",
			entry:  &config.RepoEntry{Workspace: "personal"},
			cfg:    &config.Config{Workspace: "tuck"},
			prefix: "tuck-api",
			want:   "personal",
		},
		{
			name:   "RepoEntry.Workspace alone (no .gortex.yaml)",
			entry:  &config.RepoEntry{Workspace: "personal"},
			prefix: "tuck-api",
			want:   "personal",
		},
		{
			name:   "RepoEntry empty Workspace falls through to .gortex.yaml",
			entry:  &config.RepoEntry{}, // Workspace == ""
			cfg:    &config.Config{Workspace: "tuck"},
			prefix: "tuck-api",
			want:   "tuck",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWorkspaceID(tc.entry, tc.cfg, tc.prefix)
			if got != tc.want {
				t.Errorf("resolveWorkspaceID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveProjectID_Precedence(t *testing.T) {
	if got := resolveProjectID(&config.RepoEntry{Project: "api"}, &config.Config{Project: "core"}, "tuck-api"); got != "api" {
		t.Errorf("RepoEntry.Project must win: got %q, want %q", got, "api")
	}
	if got := resolveProjectID(nil, &config.Config{Project: "core"}, "tuck-api"); got != "core" {
		t.Errorf(".gortex.yaml falls through: got %q, want %q", got, "core")
	}
	if got := resolveProjectID(nil, nil, "tuck-api"); got != "tuck-api" {
		t.Errorf("default to prefix: got %q, want %q", got, "tuck-api")
	}
}
