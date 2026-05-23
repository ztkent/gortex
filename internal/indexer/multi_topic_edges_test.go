package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// Indexer-level coverage for the EdgeProducesTopic /
// EdgeConsumesTopic / KindTopic emission introduced by the
// message-broker contract pairing. These tests build a multi-repo
// workspace, run the contracts pipeline through ReconcileContractEdges,
// and assert the topic node + edge shape downstream tooling will see.

// findTopicNode walks the graph for a KindTopic node by ID and
// returns it (or nil if absent). Used by topic-edge tests to assert
// node materialisation alongside edge presence.
func findTopicNode(g *graph.Graph, id string) *graph.Node {
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindTopic && n.ID == id {
			return n
		}
	}
	return nil
}

// collectTopicEdges returns every produces_topic / consumes_topic
// edge in the graph as "from→to" strings, for diagnostic output.
func collectTopicEdges(g *graph.Graph, kind graph.EdgeKind) []string {
	var out []string
	for _, e := range g.AllEdges() {
		if e.Kind == kind {
			out = append(out, fmt.Sprintf("%s→%s", e.From, e.To))
		}
	}
	return out
}

// TestReconcileContractEdges_TopicEdges_KafkaPair verifies the
// happy path: a Kafka producer in one repo and a Kafka consumer in
// another (both inside the shared workspace) materialise:
//
//  1. one KindTopic node `topic::kafka::user.created` with Meta
//     carrying broker + name
//  2. one EdgeProducesTopic from the producer symbol to the topic
//  3. one EdgeConsumesTopic from the consumer symbol to the topic
//
// alongside the pre-existing EdgeMatches bridge.
func TestReconcileContractEdges_TopicEdges_KafkaPair(t *testing.T) {
	pubRoot := setupGoTopicPublisherRepo(t, "producer-svc", "user.created")
	subRoot := setupGoTopicSubscriberRepo(t, "consumer-svc", "user.created")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: pubRoot, Name: "producer-svc"},
			{Path: subRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	const topicID = "topic::kafka::user.created"
	pub := "producer-svc/pub.go::publishEvent"
	sub := "consumer-svc/sub.go::consumeEvent"

	// Topic node materialised with broker meta.
	tn := findTopicNode(g, topicID)
	require.NotNilf(t, tn, "expected KindTopic node %q to exist; topic nodes in graph: %v",
		topicID, topicNodeIDs(g))
	assert.Equal(t, "kafka", tn.Meta["broker"], "topic node broker meta")
	assert.Equal(t, "user.created", tn.Meta["name"], "topic node name meta")

	// EdgeProducesTopic from the producer's symbol → topic node.
	var producesEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeProducesTopic && e.From == pub && e.To == topicID {
			producesEdge = e
			break
		}
	}
	require.NotNilf(t, producesEdge,
		"expected EdgeProducesTopic %s → %s; present produces edges: %v",
		pub, topicID, collectTopicEdges(g, graph.EdgeProducesTopic))

	// EdgeConsumesTopic from the consumer's symbol → topic node.
	var consumesEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeConsumesTopic && e.From == sub && e.To == topicID {
			consumesEdge = e
			break
		}
	}
	require.NotNilf(t, consumesEdge,
		"expected EdgeConsumesTopic %s → %s; present consumes edges: %v",
		sub, topicID, collectTopicEdges(g, graph.EdgeConsumesTopic))
	assert.True(t, consumesEdge.CrossRepo, "consumer & producer live in different repos")
}

// TestReconcileContractEdges_TopicEdges_CrossBrokerIsolation
// verifies that a Kafka producer and a NATS consumer on the same
// raw topic name do NOT pair: the Contract.IDs differ in the broker
// segment, so the matcher buckets them separately and no
// EdgeProducesTopic / EdgeConsumesTopic edges are emitted that
// bridge them.
func TestReconcileContractEdges_TopicEdges_CrossBrokerIsolation(t *testing.T) {
	pubRoot := setupGoKafkaProducerRepo(t, "producer-svc", "shared.name")
	subRoot := setupGoNATSConsumerRepo(t, "consumer-svc", "shared.name")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: pubRoot, Name: "producer-svc"},
			{Path: subRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	// Neither cross-broker topic should pair: each side becomes an
	// orphan. The graph must not contain a topic node whose
	// produces+consumes edges bridge the two repos.
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeProducesTopic && e.Kind != graph.EdgeConsumesTopic {
			continue
		}
		// EdgeProducesTopic / EdgeConsumesTopic land only when the
		// matcher pairs. Any such edge in this fixture is a
		// regression in cross-broker isolation.
		t.Errorf("unexpected topic edge across brokers: %s %s → %s",
			e.Kind, e.From, e.To)
	}
}

// TestReconcileContractEdges_TopicEdges_MultiConsumerFanout
// verifies that one Kafka producer with three Kafka consumers (each
// in its own repo, all in the shared workspace) produces three
// consume edges into a single topic node, plus one produce edge.
func TestReconcileContractEdges_TopicEdges_MultiConsumerFanout(t *testing.T) {
	pubRoot := setupGoTopicPublisherRepo(t, "producer-svc", "orders.created")
	sub1 := setupGoTopicSubscriberRepo(t, "consumer-a", "orders.created")
	sub2 := setupGoTopicSubscriberRepo(t, "consumer-b", "orders.created")
	sub3 := setupGoTopicSubscriberRepo(t, "consumer-c", "orders.created")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: pubRoot, Name: "producer-svc"},
			{Path: sub1, Name: "consumer-a"},
			{Path: sub2, Name: "consumer-b"},
			{Path: sub3, Name: "consumer-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	const topicID = "topic::kafka::orders.created"
	require.NotNil(t, findTopicNode(g, topicID), "expected KindTopic node %q", topicID)

	// One produce edge.
	produceCount := 0
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeProducesTopic && e.To == topicID {
			produceCount++
		}
	}
	assert.Equal(t, 1, produceCount, "expected exactly one EdgeProducesTopic into %s; produce edges in graph: %v",
		topicID, collectTopicEdges(g, graph.EdgeProducesTopic))

	// Three consume edges — one per consumer repo.
	consumerSyms := map[string]bool{
		"consumer-a/sub.go::consumeEvent": false,
		"consumer-b/sub.go::consumeEvent": false,
		"consumer-c/sub.go::consumeEvent": false,
	}
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeConsumesTopic || e.To != topicID {
			continue
		}
		consumerSyms[e.From] = true
	}
	for sym, seen := range consumerSyms {
		assert.Truef(t, seen, "expected EdgeConsumesTopic from %s; consume edges: %v",
			sym, collectTopicEdges(g, graph.EdgeConsumesTopic))
	}
}

// TestReconcileContractEdges_TopicEdges_CrossWorkspaceIsolation
// verifies that a producer and consumer in different workspaces
// (each repo gets its own default workspace = repo-name) do NOT
// pair, and consequently no produce/consume topic edges form.
func TestReconcileContractEdges_TopicEdges_CrossWorkspaceIsolation(t *testing.T) {
	// Build per-repo trees WITHOUT writing the shared .gortex.yaml
	// — each repo gets its own default workspace (the repo name).
	pubRoot := setupGoTopicPublisherRepo_NoSharedWorkspace(t, "producer-svc", "user.created")
	subRoot := setupGoTopicSubscriberRepo_NoSharedWorkspace(t, "consumer-svc", "user.created")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: pubRoot, Name: "producer-svc"},
			{Path: subRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeProducesTopic || e.Kind == graph.EdgeConsumesTopic {
			t.Errorf("unexpected topic edge across workspaces: %s %s → %s",
				e.Kind, e.From, e.To)
		}
	}
}

// topicNodeIDs returns the ID of every KindTopic node in the graph.
func topicNodeIDs(g *graph.Graph) []string {
	var out []string
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindTopic {
			out = append(out, n.ID)
		}
	}
	return out
}

// setupGoKafkaProducerRepo writes a minimal Go package whose
// publisher uses the Kafka-tagged confluent-style `.Produce("...")`
// idiom — the contract is tagged kafka::<topic>.
func setupGoKafkaProducerRepo(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pub.go"), []byte(
		"package main\n\nfunc publishEvent(p Producer) {\n\tp.Produce(\""+topic+"\", nil)\n}\n\ntype Producer interface{ Produce(topic string, v any) error }\n",
	), 0o644))
	return dir
}

// setupGoNATSConsumerRepo writes a Go package whose consumer uses
// `nc.Subscribe("...")` — the contract is tagged nats::<topic>, so
// pairing with a Kafka producer must fail.
func setupGoNATSConsumerRepo(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub.go"), []byte(
		"package main\n\nfunc consumeEvent(nc Conn) {\n\tnc.Subscribe(\""+topic+"\", nil)\n}\n\ntype Conn interface{ Subscribe(subject string, h any) error }\n",
	), 0o644))
	return dir
}

// setupGoTopicPublisherRepo_NoSharedWorkspace mirrors
// setupGoTopicPublisherRepo but skips the .gortex.yaml drop, so the
// repo defaults to its own workspace.
func setupGoTopicPublisherRepo_NoSharedWorkspace(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pub.go"), []byte(
		"package main\n\nfunc publishEvent(p Producer) {\n\tp.Produce(\""+topic+"\", nil)\n}\n\ntype Producer interface{ Produce(topic string, v any) error }\n",
	), 0o644))
	return dir
}

// setupGoTopicSubscriberRepo_NoSharedWorkspace mirrors
// setupGoTopicSubscriberRepo but skips the .gortex.yaml drop.
func setupGoTopicSubscriberRepo_NoSharedWorkspace(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub.go"), []byte(
		"package main\n\nfunc consumeEvent(c Consumer) {\n\tc.SubscribeTopics([]string{\""+topic+"\"}, nil)\n}\n\ntype Consumer interface{ SubscribeTopics(topics []string, rb any) error }\n",
	), 0o644))
	return dir
}
