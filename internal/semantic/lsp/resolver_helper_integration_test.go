package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestResolverHelper_RealTsserver_DefinitionAcrossFiles spins up a
// real typescript-language-server against a tiny on-disk TS fixture
// and asserts the helper resolves a cross-file method call to the
// correct declaration. Skips when typescript-language-server isn't
// on PATH (CI / dev machines without npm install).
//
// This is the load-bearing N5 integration check: the unit tests in
// resolver_registry_test.go cover dispatch logic with a scripted
// stub; this test verifies the underlying LSP-protocol wiring
// (initialize → didOpen → textDocument/definition → response) lands
// on a real graph file path.
func TestResolverHelper_RealTsserver_DefinitionAcrossFiles(t *testing.T) {
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not on PATH — skip integration test (run `npm i -g typescript-language-server typescript` to enable)")
	}

	workspace := t.TempDir()
	mustWrite(t, filepath.Join(workspace, "tsconfig.json"), `{"compilerOptions":{"target":"ES2020","module":"commonjs","strict":false}}`)
	// Use a method on a class to avoid the import-binding ambiguity:
	// tsserver's textDocument/definition on a method invocation
	// reliably returns the method declaration, even with TS's
	// declaration-merging.
	mustWrite(t, filepath.Join(workspace, "lib.ts"), `export class Worker {
  doWork(x: number): number {
    return x + 1;
  }
}
`)
	mustWrite(t, filepath.Join(workspace, "caller.ts"), `import { Worker } from "./lib";

export function callIt(): number {
  const w = new Worker();
  return w.doWork(42);
}
`)

	spec := SpecByName("typescript-language-server")
	require.NotNil(t, spec, "TS spec must be in registry")

	provider := NewProviderFromSpec(spec, zap.NewNop())
	helper := NewResolverHelper(provider, workspace, 10*time.Second, zap.NewNop())
	defer func() { _ = helper.Close() }()

	// Warm tsserver up by asking once and discarding the result —
	// the workspace project graph loads asynchronously and the first
	// definition request often races the workspace warmup. A retry
	// loop tolerates 1-2 cold attempts.
	var (
		defPath string
		defLine int
		ok      bool
	)
	deadline := time.Now().Add(8 * time.Second)
	for {
		defPath, defLine, ok = helper.Definition("caller.ts", 5, "doWork")
		if ok && defPath == "lib.ts" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	require.True(t, ok, "tsserver should eventually resolve doWork across files")
	assert.Equal(t, "lib.ts", defPath, "definition lives in lib.ts")
	// lib.ts: line 1 = `export class Worker {`, line 2 = `  doWork(...) {`
	assert.Equal(t, 2, defLine)
}

// TestResolverHelper_RealTsserver_NoMatchReturnsFalse — when the
// identifier on the requested line doesn't resolve to anything
// (typo, missing import), the helper returns ok=false rather than
// inventing a location.
func TestResolverHelper_RealTsserver_NoMatchReturnsFalse(t *testing.T) {
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		t.Skip("typescript-language-server not on PATH")
	}

	workspace := t.TempDir()
	mustWrite(t, filepath.Join(workspace, "tsconfig.json"), `{"compilerOptions":{"target":"ES2020","module":"commonjs","strict":false}}`)
	mustWrite(t, filepath.Join(workspace, "foo.ts"), `// no identifiers worth resolving here
const a = 1;
`)

	spec := SpecByName("typescript-language-server")
	provider := NewProviderFromSpec(spec, zap.NewNop())
	helper := NewResolverHelper(provider, workspace, 5*time.Second, zap.NewNop())
	defer func() { _ = helper.Close() }()

	// "ghostFunction" doesn't appear on line 2 — tsserver should
	// return an empty location set, the helper should report
	// ok=false, the resolver falls through to heuristics.
	_, _, ok := helper.Definition("foo.ts", 2, "ghostFunction")
	assert.False(t, ok)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}
