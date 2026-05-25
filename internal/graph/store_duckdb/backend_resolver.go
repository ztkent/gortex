package store_duckdb

// Phase 1 stubs for the expanded BackendResolver interface. See
// store_kuzu/backend_resolver.go for the contract. Per-rule SQL
// lands in later phases.

func (s *Store) ResolveSameFile() (int, error)              { return 0, nil }
func (s *Store) ResolveSamePackage() (int, error)           { return 0, nil }
func (s *Store) ResolveImportAware() (int, error)           { return 0, nil }
func (s *Store) ResolveRelativeImports(string) (int, error) { return 0, nil }
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
func (s *Store) ResolveExternalCallStubs() (int, error)     { return 0, nil }

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
