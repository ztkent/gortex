package wiki

// (Real Enhancer implementations live in enhancer_claudecli.go.)
//
// This file exists so the package always compiles even when no
// provider build constraint is satisfied — only NoopEnhancer is needed
// for tests and for the --enhance=false default path.
