package forest

import "errors"

// errNilLanguage is returned when a grammar's GetLanguage() returns
// nil. Sticky: cached on the Extractor, never retried.
var errNilLanguage = errors.New("forest: grammar returned nil language pointer")
