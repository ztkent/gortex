package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDockerfileExtractor_FromImports(t *testing.T) {
	src := []byte(`FROM golang:1.21 AS builder
FROM alpine:3.18
`)
	e := NewDockerfileExtractor()
	result, err := e.Extract("app.dockerfile", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.GreaterOrEqual(t, len(imports), 1, "should extract FROM as imports")
}

func TestDockerfileExtractor_EnvAndArg(t *testing.T) {
	src := []byte(`FROM ubuntu:22.04
ARG VERSION=1.0
ENV APP_HOME=/app
COPY . /app
RUN make build
CMD ["./app"]
`)
	e := NewDockerfileExtractor()
	result, err := e.Extract("build.dockerfile", src)
	require.NoError(t, err)

	// Should have file node + variables.
	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 2, "should extract ARG, ENV, and instructions")
}

func TestDockerfileExtractor_FileNode(t *testing.T) {
	src := []byte(`FROM scratch
`)
	e := NewDockerfileExtractor()
	result, err := e.Extract("minimal.dockerfile", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	assert.Equal(t, 1, len(files))
	assert.Equal(t, "minimal.dockerfile", files[0].Name)
}

func TestDockerfileExtractor_RunLineNodes(t *testing.T) {
	src := []byte(`FROM golang:1.22

RUN apk add --no-cache make
RUN go mod download
RUN make build
`)
	e := NewDockerfileExtractor()
	result, err := e.Extract("Dockerfile", src)
	if err != nil {
		t.Fatal(err)
	}

	runs := []string{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFunction && n.Language == "dockerfile" {
			runs = append(runs, n.Name)
		}
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 RUN function nodes, got %d (%v)", len(runs), runs)
	}
	for _, name := range runs {
		if !strings.HasPrefix(name, "run-line-") {
			t.Errorf("expected run-line-* name, got %q", name)
		}
	}
}
