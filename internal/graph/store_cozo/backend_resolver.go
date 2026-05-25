//go:build cozo

package store_cozo

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store satisfies graph.BackendResolver.
var _ graph.BackendResolver = (*Store)(nil)

// Phase 1 stubs for the expanded BackendResolver interface. Datalog
// implementations land in Phase 4a.

func (s *Store) ResolveSameFile() (int, error)              { return 0, nil }
func (s *Store) ResolveSamePackage() (int, error)           { return 0, nil }
func (s *Store) ResolveImportAware() (int, error)           { return 0, nil }
func (s *Store) ResolveRelativeImports(string) (int, error) { return 0, nil }
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
func (s *Store) ResolveUniqueNames() (int, error)           { return 0, nil }
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
