package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// storeActionCallers returns the names of functions that call a store action
// (a KindFunction node named `member` carrying Meta["store_factory"]).
func storeActionCallers(g *graph.Graph, member string) []string {
	var callers []string
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction || n.Meta == nil {
			continue
		}
		if _, ok := n.Meta["store_factory"]; !ok {
			continue
		}
		if sm, _ := n.Meta["store_member"].(string); sm != member {
			continue
		}
		for _, e := range g.GetInEdges(n.ID) {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			if cn := g.GetNode(e.From); cn != nil {
				callers = append(callers, cn.Name)
			}
		}
	}
	return callers
}

func indexFixture(t *testing.T, files map[string]string) *graph.Graph {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	return g
}

// TestStoreFactory_Zustand_TS mirrors the competitor's end-to-end test: a
// Zustand store consumed cross-file via the getState() chain and via
// destructuring. Both indirect calls must resolve to the action nodes.
func TestStoreFactory_Zustand_TS(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"store.ts": `import { create } from 'zustand'

export const useStore = create((set, get) => ({
  fetchUser: async () => {},
  reset: () => {},
}))
`,
		"caller.ts": `import { useStore } from './store'

export function hardReset() {
  useStore.getState().reset()
}

export function loginFlow() {
  const { fetchUser } = useStore.getState()
  fetchUser()
}
`,
	})

	resetCallers := storeActionCallers(g, "reset")
	require.Contains(t, resetCallers, "hardReset", "chained useStore.getState().reset() should resolve")

	fetchCallers := storeActionCallers(g, "fetchUser")
	require.Contains(t, fetchCallers, "loginFlow", "destructured fetchUser() should resolve")
}

// TestStoreFactory_Zustand_JS covers the JavaScript extractor with a
// middleware-wrapped store factory.
func TestStoreFactory_Zustand_JS(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"store.js": `import { create } from 'zustand'
import { persist } from 'zustand/middleware'

export const useStore = create(persist((set, get) => ({
  increment: () => {},
}), { name: 'c' }))
`,
		"caller.js": `import { useStore } from './store'

export function bump() {
  useStore.getState().increment()
}
`,
	})
	require.Contains(t, storeActionCallers(g, "increment"), "bump",
		"middleware-wrapped store action should resolve through the getState() chain")
}

func hasStoreAction(g *graph.Graph, member string) bool {
	for _, n := range g.AllNodes() {
		if n.Meta == nil {
			continue
		}
		if _, ok := n.Meta["store_factory"]; !ok {
			continue
		}
		if sm, _ := n.Meta["store_member"].(string); sm == member {
			return true
		}
	}
	return false
}

// TestStoreFactory_PiniaAndRTK covers the non-function-return factory shapes:
// Pinia's defineStore('id', {actions:{...}}) and Redux Toolkit's
// createSlice({reducers:{...}}).
func TestStoreFactory_PiniaAndRTK(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"pinia.ts": `import { defineStore } from 'pinia'

export const useS = defineStore('s', {
  actions: {
    save() {},
  },
})
`,
		"slice.ts": `import { createSlice } from '@reduxjs/toolkit'

export const slice = createSlice({
  name: 'counter',
  reducers: {
    tick() {},
  },
})
`,
	})
	require.True(t, hasStoreAction(g, "save"), "Pinia defineStore actions should be stamped as store actions")
	require.True(t, hasStoreAction(g, "tick"), "RTK createSlice reducers should be stamped as store actions")
}
