package indexer

import (
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Reference-facts sidecar persistence. After resolution settles, each resolved
// reference edge becomes a durable, auditable fact (from → to + provenance
// tier) persisted per source file in the backend's ref_facts table. Only the
// on-disk store implements graph.RefFactsWriter; the in-memory backend has no
// durable layer to seed, so persistence is a no-op there (the live edges ARE
// the facts).

// refFactsWriter returns the backend's reference-facts persistence capability
// if it implements one.
func (idx *Indexer) refFactsWriter() (graph.RefFactsWriter, bool) {
	w, ok := idx.graph.(graph.RefFactsWriter)
	return w, ok
}

// collectRefFacts derives the resolved-reference facts originating in the given
// nodes: every resolvable reference edge whose target is a concrete (non-stub,
// non-unresolved) node.
func collectRefFacts(g graph.Store, nodes []*graph.Node) []graph.RefFact {
	var facts []graph.RefFact
	for _, n := range nodes {
		if n == nil {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e == nil || !graph.IsResolvableRefEdge(e.Kind) {
				continue
			}
			if e.To == "" || graph.IsUnresolvedTarget(e.To) || graph.IsStub(e.To) {
				continue
			}
			refName := ""
			if t := g.GetNode(e.To); t != nil {
				refName = t.Name
			}
			origin := e.Origin
			if origin == "" {
				sem, _ := e.Meta["semantic_source"].(string)
				origin = graph.DefaultOriginFor(e.Kind, e.Confidence, sem)
			}
			facts = append(facts, graph.RefFact{
				RepoPrefix: n.RepoPrefix,
				FromID:     e.From,
				ToID:       e.To,
				Kind:       string(e.Kind),
				RefName:    refName,
				Line:       e.Line,
				Origin:     origin,
				Tier:       graph.ResolvedBy(origin),
				FilePath:   n.FilePath,
				Lang:       n.Language,
			})
		}
	}
	return facts
}

// persistRefFactsForFiles re-derives and persists the resolved-reference facts
// for the given graph file paths (delete-then-set per file so stale facts from
// removed references don't linger). No-op when the backend has no durable layer
// or the file list is empty.
func (idx *Indexer) persistRefFactsForFiles(graphPaths []string) {
	w, ok := idx.refFactsWriter()
	if !ok || len(graphPaths) == 0 {
		return
	}
	byRepo := map[string][]graph.RefFact{}
	filesByRepo := map[string]map[string]struct{}{}
	for _, p := range graphPaths {
		nodes := idx.graph.GetFileNodes(p)
		for _, f := range collectRefFacts(idx.graph, nodes) {
			byRepo[f.RepoPrefix] = append(byRepo[f.RepoPrefix], f)
			if filesByRepo[f.RepoPrefix] == nil {
				filesByRepo[f.RepoPrefix] = map[string]struct{}{}
			}
			filesByRepo[f.RepoPrefix][f.FilePath] = struct{}{}
		}
	}
	for repo, fileSet := range filesByRepo {
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		if err := w.DeleteRefFactsByFiles(repo, files); err != nil {
			idx.logger.Debug("ref-facts: delete failed", zap.Error(err))
			continue
		}
		if err := w.BulkSetRefFacts(repo, byRepo[repo]); err != nil {
			idx.logger.Debug("ref-facts: persist failed", zap.Error(err))
		}
	}
}

// persistAllRefFacts persists reference facts for every indexed file. Called
// once after a full resolve so a cold index seeds the durable sidecar.
func (idx *Indexer) persistAllRefFacts() {
	if _, ok := idx.refFactsWriter(); !ok {
		return
	}
	var files []string
	for n := range idx.graph.NodesByKind(graph.KindFile) {
		if n != nil {
			files = append(files, n.ID)
		}
	}
	idx.persistRefFactsForFiles(files)
}

// deleteRefFactsForFiles drops persisted facts sourced in the given graph file
// paths (used when a file is evicted/deleted). No-op without a durable backend.
func (idx *Indexer) deleteRefFactsForFiles(repoPrefix string, graphPaths []string) {
	w, ok := idx.refFactsWriter()
	if !ok || len(graphPaths) == 0 {
		return
	}
	if err := w.DeleteRefFactsByFiles(repoPrefix, graphPaths); err != nil {
		idx.logger.Debug("ref-facts: delete-on-evict failed", zap.Error(err))
	}
}
