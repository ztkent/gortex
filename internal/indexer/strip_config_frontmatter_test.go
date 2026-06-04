package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestStripConfigArtifacts_PreservesDocFrontmatter verifies the strip pass
// (run when the `configs` coverage domain is gated off, the default) drops
// code-origin config keys but keeps document-frontmatter keys — so a
// Quarto .qmd's declared metadata stays searchable.
func TestStripConfigArtifacts_PreservesDocFrontmatter(t *testing.T) {
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{
			{ID: "cfg::env::AWS_SECRET", Kind: graph.KindConfigKey, Name: "AWS_SECRET", Meta: map[string]any{"source": "env"}},
			{ID: "report.qmd::cfg:title", Kind: graph.KindConfigKey, Name: "title", Meta: map[string]any{"source": "quarto_frontmatter"}},
			{ID: "k8s::cfg::REPLICAS", Kind: graph.KindConfigKey, Name: "REPLICAS", Meta: map[string]any{"origin": "k8s"}},
			{ID: "report.qmd", Kind: graph.KindFile, Name: "report.qmd"},
		},
		Edges: []*graph.Edge{
			{From: "report.qmd", To: "report.qmd::cfg:title", Kind: graph.EdgeDefines},
			{From: "svc.go::Load", To: "cfg::env::AWS_SECRET", Kind: graph.EdgeReadsConfig},
		},
	}
	stripConfigArtifacts(result)

	has := func(id string) bool {
		for _, n := range result.Nodes {
			if n.ID == id {
				return true
			}
		}
		return false
	}
	if has("cfg::env::AWS_SECRET") {
		t.Error("code-origin config key should be stripped when the configs domain is off")
	}
	if !has("report.qmd::cfg:title") {
		t.Error("quarto frontmatter config key must survive the configs-domain strip")
	}
	if !has("k8s::cfg::REPLICAS") {
		t.Error("infra-origin config key must survive the strip")
	}

	defines, reads := 0, 0
	for _, e := range result.Edges {
		switch e.Kind {
		case graph.EdgeDefines:
			defines++
		case graph.EdgeReadsConfig:
			reads++
		}
	}
	if defines != 1 {
		t.Errorf("EdgeDefines to surviving frontmatter key = %d, want 1", defines)
	}
	if reads != 0 {
		t.Errorf("reads_config edges = %d, want 0 (dropped with the domain)", reads)
	}
}

// TestIndex_QuartoFrontmatterSurvivesDefaultConfig is the end-to-end guard:
// indexing a .qmd with the default config (configs domain off) still yields
// the frontmatter keys as graph nodes.
func TestIndex_QuartoFrontmatterSurvivesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "report.qmd"), `---
title: My Report
format: html
---

# Introduction

Some prose.
`)
	reg := parser.NewRegistry()
	reg.Register(languages.NewQuartoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2

	g := graph.New()
	idx := New(g, reg, cfg, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	if len(g.FindNodesByName("title")) == 0 {
		t.Error("quarto frontmatter key 'title' missing from default-config index")
	}
	if len(g.FindNodesByName("format")) == 0 {
		t.Error("quarto frontmatter key 'format' missing from default-config index")
	}
}
