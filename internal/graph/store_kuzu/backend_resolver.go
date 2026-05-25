package store_kuzu

// Phase 1 stubs for the expanded BackendResolver interface. Each
// returns (0, nil) until the per-rule Cypher implementation lands in
// later phases (Phase 2 ships ResolveSameFile / ResolveSamePackage /
// ResolveImportAware, Phase 3 ships the rest). ResolveUniqueNames
// remains the existing Cypher pass — see store.go.

func (s *Store) ResolveSameFile() (int, error)              { return 0, nil }
func (s *Store) ResolveSamePackage() (int, error)           { return 0, nil }
func (s *Store) ResolveImportAware() (int, error)           { return 0, nil }
func (s *Store) ResolveRelativeImports(string) (int, error) { return 0, nil }
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
func (s *Store) ResolveExternalCallStubs() (int, error)     { return 0, nil }

// ResolveAllBulk chains every backend-resolver rule in precision-
// descending order and sums the resolved counts. Errors from a
// single rule are non-fatal; the orchestrator logs internally and
// continues so a buggy rule can't block the others.
func (s *Store) ResolveAllBulk() (int, error) {
	var total int
	for _, fn := range []func() (int, error){
		s.ResolveSameFile,
		s.ResolveSamePackage,
		s.ResolveImportAware,
		func() (int, error) { return s.ResolveRelativeImports("") },
		s.ResolveCrossRepo,
		s.ResolveUniqueNames,
		s.ResolveExternalCallStubs,
	} {
		n, err := fn()
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
