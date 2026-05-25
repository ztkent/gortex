package store_ladybug

// Phase 1 stubs for the expanded BackendResolver interface. Ladybug
// is a Kuzu fork; per-rule Cypher will mirror the Kuzu
// implementations in later phases.

func (s *Store) ResolveSameFile() (int, error)              { return 0, nil }
func (s *Store) ResolveSamePackage() (int, error)           { return 0, nil }
func (s *Store) ResolveImportAware() (int, error)           { return 0, nil }
func (s *Store) ResolveRelativeImports(string) (int, error) { return 0, nil }
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
func (s *Store) ResolveExternalCallStubs() (int, error)     { return 0, nil }

// ResolveUniqueNames lives in store.go (the existing per-call
// MERGE implementation Ladybug inherited from Kuzu). Phase 2+ will
// replace it with the Cypher fork-of-Kuzu pass.

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
