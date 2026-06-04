package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	gsql "github.com/zzet/gortex/internal/sql"
)

var (
	dbPostgresDSN string
	dbSchemaName  string
	dbOut         string
)

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Live database connectors",
	Long:  "Ingest schema from a live database into the Gortex graph.",
}

var dbSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Introspect a live database and emit its schema as CREATE TABLE DDL",
	Long: "Connects to a live database via --postgres <dsn>, reads its tables, columns, primary keys, and foreign keys from information_schema, and writes standard CREATE TABLE DDL.\n\n" +
		"Gortex's SQL extractor ingests that DDL into the graph as db::<dialect>:: table and column nodes the same way it ingests hand-written migrations — so a live database becomes queryable graph nodes with no separate ingestion path. Write the output into a tracked repo with --out and the daemon picks it up on its next index, cross-referencing the live schema with the code that queries it.",
	RunE: runDBSchema,
}

func init() {
	dbSchemaCmd.Flags().StringVar(&dbPostgresDSN, "postgres", "", "PostgreSQL DSN (e.g. postgres://user:pass@host:5432/dbname)")
	dbSchemaCmd.Flags().StringVar(&dbSchemaName, "schema", "public", "Schema to introspect (\"*\" for all non-system schemas)")
	dbSchemaCmd.Flags().StringVar(&dbOut, "out", "", "Write DDL to this file (default: stdout)")
	dbCmd.AddCommand(dbSchemaCmd)
	rootCmd.AddCommand(dbCmd)
}

func runDBSchema(cmd *cobra.Command, _ []string) error {
	if dbPostgresDSN == "" {
		return fmt.Errorf("a connector is required: pass --postgres <dsn>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ls, err := gsql.IntrospectPostgres(ctx, dbPostgresDSN, dbSchemaName)
	if err != nil {
		return err
	}
	ddl := ls.ToDDL()

	if dbOut == "" {
		_, err := fmt.Fprint(cmd.OutOrStdout(), ddl)
		return err
	}
	if err := os.WriteFile(dbOut, []byte(ddl), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dbOut, err)
	}
	tables := map[string]struct{}{}
	for _, c := range ls.Columns {
		tables[c.Schema+"."+c.Table] = struct{}{}
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote schema for %d tables to %s\n", len(tables), dbOut)
	return nil
}
