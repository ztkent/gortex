package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func nodeByID(nodes []*graph.Node, id string) *graph.Node {
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func colNames(nodes []*graph.Node) []string {
	var out []string
	for _, n := range nodes {
		if n.Kind == graph.KindColumn {
			out = append(out, n.Name)
		}
	}
	return out
}

// --- dbt SQL models -------------------------------------------------------

func TestDbt_SQLModel_RefSourceConfigColumns(t *testing.T) {
	src := []byte(`{{ config(materialized='table', schema='analytics') }}

with base as (
    select * from {{ source('raw', 'events') }}
)
select
    e.event_id,
    e.user_id,
    count(*) as event_count
from base e
join {{ ref('dim_users') }} u on u.id = e.user_id
group by 1, 2
`)
	e := NewSQLExtractor()
	result, err := e.Extract("models/staging/stg_events.sql", src)
	require.NoError(t, err)

	model := nodeByID(result.Nodes, "dbt::model::stg_events")
	require.NotNil(t, model, "model node should exist")
	assert.Equal(t, graph.KindTable, model.Kind)
	assert.Equal(t, "stg_events", model.Name)
	assert.Equal(t, "dbt", model.Meta["framework"])
	assert.Equal(t, "dbt_model", model.Meta["sql_type"])
	assert.Equal(t, "model", model.Meta["resource_type"])
	assert.Equal(t, "table", model.Meta["materialized"])
	assert.Equal(t, "analytics", model.Meta["schema"])

	// Source referenced via source() is materialised as a KindTable.
	srcNode := nodeByID(result.Nodes, "dbt::source::raw.events")
	require.NotNil(t, srcNode, "source node should exist")
	assert.Equal(t, "dbt_source", srcNode.Meta["sql_type"])

	// Columns from the model's final SELECT projection.
	cols := colNames(result.Nodes)
	assert.ElementsMatch(t, []string{"event_id", "user_id", "event_count"}, cols)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 3)
	for _, me := range memberEdges {
		assert.Equal(t, "dbt::model::stg_events", me.To)
	}

	// Lineage: one ref() + one source() dependency.
	deps := edgesOfKind(result.Edges, graph.EdgeDependsOn)
	require.Len(t, deps, 2)
	targets := map[string]string{}
	for _, d := range deps {
		targets[d.To], _ = d.Meta["link"].(string)
	}
	assert.Equal(t, "ref", targets["dbt::model::dim_users"])
	assert.Equal(t, "source", targets["dbt::source::raw.events"])
}

func TestDbt_SQLModel_PureSQLByPath(t *testing.T) {
	// A dbt model need not contain any Jinja — path + a query-shaped
	// body is enough to classify it.
	src := []byte("select id, name, email from some_upstream\n")
	e := NewSQLExtractor()
	result, err := e.Extract("models/marts/dim_users.sql", src)
	require.NoError(t, err)

	model := nodeByID(result.Nodes, "dbt::model::dim_users")
	require.NotNil(t, model)
	assert.Equal(t, "view", model.Meta["materialized"]) // default
	assert.ElementsMatch(t, []string{"id", "name", "email"}, colNames(result.Nodes))
}

func TestDbt_SQLModel_Snapshot(t *testing.T) {
	src := []byte(`{% snapshot orders_snapshot %}
{{ config(target_schema='snapshots', unique_key='id', strategy='timestamp') }}
select id, status, updated_at from {{ source('jaffle', 'orders') }}
{% endsnapshot %}
`)
	e := NewSQLExtractor()
	result, err := e.Extract("snapshots/orders_snapshot.sql", src)
	require.NoError(t, err)

	model := nodeByID(result.Nodes, "dbt::model::orders_snapshot")
	require.NotNil(t, model)
	assert.Equal(t, "snapshot", model.Meta["resource_type"])
	assert.Equal(t, "snapshot", model.Meta["materialized"])
	assert.ElementsMatch(t, []string{"id", "status", "updated_at"}, colNames(result.Nodes))

	deps := edgesOfKind(result.Edges, graph.EdgeDependsOn)
	require.Len(t, deps, 1)
	assert.Equal(t, "dbt::source::jaffle.orders", deps[0].To)
}

func TestDbt_SQLModel_TwoArgRef(t *testing.T) {
	src := []byte("select 1 as id from {{ ref('analytics', 'dim_date') }}\n")
	e := NewSQLExtractor()
	result, err := e.Extract("models/x.sql", src)
	require.NoError(t, err)

	deps := edgesOfKind(result.Edges, graph.EdgeDependsOn)
	require.Len(t, deps, 1)
	assert.Equal(t, "dbt::model::dim_date", deps[0].To)
	assert.Equal(t, "analytics", deps[0].Meta["ref_package"])
}

// --- SQLMesh SQL models ---------------------------------------------------

func TestSQLMesh_Model_BlockPropsAndLineage(t *testing.T) {
	src := []byte(`MODEL (
  name sushi.customers,
  kind FULL,
  cron '@daily',
  grain customer_id,
  owner 'data-team'
);

SELECT
  customer_id,
  cast(zip AS TEXT) AS zip_code,
  status
FROM sushi.raw_customers
`)
	e := NewSQLExtractor()
	result, err := e.Extract("models/customers.sql", src)
	require.NoError(t, err)

	model := nodeByID(result.Nodes, "sqlmesh::model::sushi.customers")
	require.NotNil(t, model)
	assert.Equal(t, "customers", model.Name)
	assert.Equal(t, "sqlmesh", model.Meta["framework"])
	assert.Equal(t, "sushi", model.Meta["schema"])
	assert.Equal(t, "full", model.Meta["materialized"])
	assert.Equal(t, "@daily", model.Meta["cron"])
	assert.Equal(t, "customer_id", model.Meta["grain"])
	assert.Equal(t, "data-team", model.Meta["owner"])

	assert.ElementsMatch(t, []string{"customer_id", "zip_code", "status"}, colNames(result.Nodes))

	deps := edgesOfKind(result.Edges, graph.EdgeDependsOn)
	require.Len(t, deps, 1)
	assert.Equal(t, "sqlmesh::model::sushi.raw_customers", deps[0].To)
}

func TestSQLMesh_Model_ColumnsProperty(t *testing.T) {
	src := []byte(`MODEL (
  name analytics.orders,
  kind INCREMENTAL_BY_TIME_RANGE (
    time_column event_date
  ),
  columns (
    order_id INT,
    total DECIMAL(10, 2),
    created_at TIMESTAMP
  )
);

SELECT order_id, total, created_at
FROM analytics.staging_orders
WHERE event_date BETWEEN @start_date AND @end_date
`)
	e := NewSQLExtractor()
	result, err := e.Extract("models/orders.sql", src)
	require.NoError(t, err)

	model := nodeByID(result.Nodes, "sqlmesh::model::analytics.orders")
	require.NotNil(t, model)
	assert.Equal(t, "incremental_by_time_range", model.Meta["materialized"])

	// Explicit columns() property wins; data_type is carried in Meta.
	cols := nodesOfKind(result.Nodes, graph.KindColumn)
	require.Len(t, cols, 3)
	byName := map[string]*graph.Node{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	require.Contains(t, byName, "order_id")
	assert.Equal(t, "INT", byName["order_id"].Meta["data_type"])
	assert.Equal(t, "DECIMAL(10, 2)", byName["total"].Meta["data_type"])

	deps := edgesOfKind(result.Edges, graph.EdgeDependsOn)
	require.Len(t, deps, 1)
	assert.Equal(t, "sqlmesh::model::analytics.staging_orders", deps[0].To)
}

// --- dbt schema YAML ------------------------------------------------------

func TestDbt_SchemaYAML_ModelsAndSources(t *testing.T) {
	src := []byte(`version: 2

models:
  - name: stg_events
    description: "Staged event data"
    config:
      materialized: view
    columns:
      - name: event_id
        description: "Primary key"
        data_type: integer
        tests:
          - unique
          - not_null
      - name: user_id
        data_type: integer

sources:
  - name: raw
    schema: raw_data
    tables:
      - name: events
        columns:
          - name: id
          - name: payload
            data_type: json
`)
	e := NewYAMLExtractor()
	result, err := e.Extract("models/staging/schema.yml", src)
	require.NoError(t, err)

	// Generic top-level-keys fallback must NOT have run.
	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindVariable))

	model := nodeByID(result.Nodes, "dbt::model::stg_events")
	require.NotNil(t, model)
	assert.Equal(t, graph.KindTable, model.Kind)
	assert.Equal(t, "view", model.Meta["materialized"])
	assert.Equal(t, "Staged event data", model.Meta["description"])

	eventID := nodeByID(result.Nodes, "dbt::model::stg_events::event_id")
	require.NotNil(t, eventID)
	assert.Equal(t, graph.KindColumn, eventID.Kind)
	assert.Equal(t, "integer", eventID.Meta["data_type"])
	assert.Equal(t, "Primary key", eventID.Meta["description"])
	assert.ElementsMatch(t, []string{"unique", "not_null"}, eventID.Meta["tests"])

	source := nodeByID(result.Nodes, "dbt::source::raw.events")
	require.NotNil(t, source)
	assert.Equal(t, "dbt_source", source.Meta["sql_type"])
	assert.Equal(t, "raw_data", source.Meta["schema"])

	payload := nodeByID(result.Nodes, "dbt::source::raw.events::payload")
	require.NotNil(t, payload)
	assert.Equal(t, "json", payload.Meta["data_type"])

	assert.Len(t, nodesOfKind(result.Nodes, graph.KindColumn), 4)
	assert.Len(t, edgesOfKind(result.Edges, graph.EdgeMemberOf), 4)
}

func TestDbt_SchemaYAML_PlainConfigFallsThrough(t *testing.T) {
	// A plain config YAML must not be mistaken for a dbt schema file —
	// the generic top-level-keys walker should still run.
	src := []byte(`server:
  port: 8080
database:
  host: localhost
`)
	e := NewYAMLExtractor()
	result, err := e.Extract("config/app.yml", src)
	require.NoError(t, err)

	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindTable))
	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.Len(t, vars, 2) // server, database
}

// --- classification -------------------------------------------------------

func TestClassifySQLFile(t *testing.T) {
	cases := []struct {
		name string
		path string
		src  string
		want string
	}{
		{"sqlmesh model block", "models/a.sql", "MODEL (\n  name a\n);\nSELECT 1", "sqlmesh"},
		{"sqlmesh leading comment", "a.sql", "-- header\nMODEL (name a);\nSELECT 1", "sqlmesh"},
		{"dbt ref marker", "x.sql", "select * from {{ ref('y') }}", "dbt"},
		{"dbt config marker", "x.sql", "{{ config(materialized='table') }}\nselect 1", "dbt"},
		{"dbt pure sql by path", "models/dim.sql", "select id from upstream", "dbt"},
		{"plain DDL not dbt", "schema.sql", "CREATE TABLE foo (id int);", ""},
		{"DDL under models dir not dbt", "models/foo.sql", "CREATE TABLE foo (id int);", ""},
		{"plain query outside models", "scratch.sql", "select 1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifySQLFile(tc.path, []byte(tc.src)))
		})
	}
}

func TestSQLExtractor_NonDbtUnaffected(t *testing.T) {
	// Regression guard: a plain DDL file still goes through the generic
	// SQL walk and produces a KindType, not a dbt model node.
	src := []byte("CREATE TABLE users (id INTEGER PRIMARY KEY);")
	e := NewSQLExtractor()
	result, err := e.Extract("db/schema.sql", src)
	require.NoError(t, err)
	assert.Len(t, nodesOfKind(result.Nodes, graph.KindType), 1)
	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindTable))
}
