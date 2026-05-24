package main

import "github.com/zzet/gortex/internal/graph"

// cozoFactory is populated by cozo_register.go when the bench is
// built with -tags cozo; otherwise it stays nil and the bench loop
// skips the cozo backend. The build-tag isolation pattern exists
// because Cozo bundles Rust's libstd, and any other Rust-static-lib
// backend (lora etc.) would collide on _rust_eh_personality at link
// time.
var cozoFactory func() (graph.Store, func() int64, error)
