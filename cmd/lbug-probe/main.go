package main

import (
	"fmt"
	"os"

	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

func main() {
	path := "/tmp/lbug-fresh"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	fmt.Printf("Opening %s ...\n", path)
	s, err := store_ladybug.Open(path)
	if err != nil {
		fmt.Println("ERR:", err)
		os.Exit(1)
	}
	defer func() { _ = s.Close() }()
	fmt.Printf("OK nodes=%d edges=%d\n", s.NodeCount(), s.EdgeCount())
}
