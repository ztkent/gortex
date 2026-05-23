package contracts

import "testing"

// Topic-pairing tests covering the matcher behaviour the indexer's
// ReconcileContractEdges pass relies on: same-(broker,topic) pairs
// match, different-broker pairs don't, multi-consumer fanout produces
// one CrossLink per consumer, and the workspace boundary keeps
// unrelated repos from pairing. The actual EdgeProducesTopic /
// EdgeConsumesTopic emission is covered by indexer-level tests
// (TestReconcileContractEdges_TopicEdges*), but those depend on
// matcher correctness — these tests pin the contract behaviour the
// indexer assumes.

// TestTopicMatch_BasicPair verifies that a single Kafka producer +
// single Kafka consumer for the same topic pair into one CrossLink
// with no orphans. WorkspaceID and ProjectID are shared so neither
// the workspace nor the project boundary intervenes — pairing is
// purely on the broker-tagged Contract.ID.
func TestTopicMatch_BasicPair(t *testing.T) {
	id := "topic::kafka::user.created"
	reg := NewRegistry()
	reg.Add(Contract{
		ID:          id,
		Type:        ContractTopic,
		Role:        RoleProvider,
		SymbolID:    "producer/pub.go::publish",
		FilePath:    "pub.go",
		RepoPrefix:  "producer",
		WorkspaceID: "shared",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "user.created"},
	})
	reg.Add(Contract{
		ID:          id,
		Type:        ContractTopic,
		Role:        RoleConsumer,
		SymbolID:    "consumer/sub.go::consume",
		FilePath:    "sub.go",
		RepoPrefix:  "consumer",
		WorkspaceID: "shared",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "user.created"},
	})

	res := Match(reg)
	if len(res.Matched) != 1 {
		t.Fatalf("expected 1 matched pair, got %d", len(res.Matched))
	}
	if len(res.OrphanProviders) != 0 || len(res.OrphanConsumers) != 0 {
		t.Errorf("expected zero orphans, got providers=%d consumers=%d", len(res.OrphanProviders), len(res.OrphanConsumers))
	}
	m := res.Matched[0]
	if !m.CrossRepo {
		t.Errorf("expected CrossRepo=true (different repos in same workspace)")
	}
	if m.Provider.SymbolID != "producer/pub.go::publish" {
		t.Errorf("unexpected provider symbol: %q", m.Provider.SymbolID)
	}
	if m.Consumer.SymbolID != "consumer/sub.go::consume" {
		t.Errorf("unexpected consumer symbol: %q", m.Consumer.SymbolID)
	}
}

// TestTopicMatch_CrossBrokerIsolation verifies that a topic named
// "foo" on Kafka and a topic named "foo" on NATS do not pair: their
// Contract.IDs differ in the broker segment so the matcher's bucket
// keys keep them apart.
func TestTopicMatch_CrossBrokerIsolation(t *testing.T) {
	reg := NewRegistry()
	reg.Add(Contract{
		ID:          "topic::kafka::foo",
		Type:        ContractTopic,
		Role:        RoleProvider,
		SymbolID:    "svc-a/pub.go::publish",
		RepoPrefix:  "svc-a",
		WorkspaceID: "shared",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "foo"},
	})
	reg.Add(Contract{
		ID:          "topic::nats::foo",
		Type:        ContractTopic,
		Role:        RoleConsumer,
		SymbolID:    "svc-b/sub.go::consume",
		RepoPrefix:  "svc-b",
		WorkspaceID: "shared",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "nats", "topic": "foo"},
	})

	res := Match(reg)
	if len(res.Matched) != 0 {
		t.Errorf("expected zero matched pairs (cross-broker), got %d", len(res.Matched))
	}
	if len(res.OrphanProviders) != 1 || len(res.OrphanConsumers) != 1 {
		t.Errorf("expected 1 orphan provider + 1 orphan consumer, got %d / %d",
			len(res.OrphanProviders), len(res.OrphanConsumers))
	}
}

// TestTopicMatch_MultiConsumerFanout verifies that one producer with
// three consumers on the same topic yields three CrossLinks (one
// per consumer).
func TestTopicMatch_MultiConsumerFanout(t *testing.T) {
	reg := NewRegistry()
	id := "topic::kafka::orders.created"
	reg.Add(Contract{
		ID:          id,
		Type:        ContractTopic,
		Role:        RoleProvider,
		SymbolID:    "producer/pub.go::publish",
		RepoPrefix:  "producer",
		WorkspaceID: "shared",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "orders.created"},
	})
	for i, name := range []string{"a", "b", "c"} {
		_ = i
		reg.Add(Contract{
			ID:          id,
			Type:        ContractTopic,
			Role:        RoleConsumer,
			SymbolID:    "consumer-" + name + "/sub.go::consume",
			RepoPrefix:  "consumer-" + name,
			WorkspaceID: "shared",
			ProjectID:   "shared",
			Meta:        map[string]any{"broker": "kafka", "topic": "orders.created"},
		})
	}

	res := Match(reg)
	if len(res.Matched) != 3 {
		t.Fatalf("expected 3 matched pairs (fanout), got %d", len(res.Matched))
	}
	if len(res.OrphanProviders) != 0 || len(res.OrphanConsumers) != 0 {
		t.Errorf("expected zero orphans, got providers=%d consumers=%d",
			len(res.OrphanProviders), len(res.OrphanConsumers))
	}
	gotConsumers := make(map[string]bool)
	for _, m := range res.Matched {
		gotConsumers[m.Consumer.SymbolID] = true
		if m.Provider.SymbolID != "producer/pub.go::publish" {
			t.Errorf("matched pair has wrong provider: %q", m.Provider.SymbolID)
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		want := "consumer-" + name + "/sub.go::consume"
		if !gotConsumers[want] {
			t.Errorf("expected matched consumer %q to appear", want)
		}
	}
}

// TestTopicMatch_CrossWorkspaceIsolation verifies that a producer in
// workspace A and a consumer in workspace B for the same topic do
// not pair. The matcher's bucket key includes EffectiveWorkspace,
// so different workspaces yield different buckets and each side
// becomes an orphan.
func TestTopicMatch_CrossWorkspaceIsolation(t *testing.T) {
	reg := NewRegistry()
	id := "topic::kafka::user.created"
	reg.Add(Contract{
		ID:          id,
		Type:        ContractTopic,
		Role:        RoleProvider,
		SymbolID:    "ws-a/pub.go::publish",
		RepoPrefix:  "ws-a",
		WorkspaceID: "workspace-a",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "user.created"},
	})
	reg.Add(Contract{
		ID:          id,
		Type:        ContractTopic,
		Role:        RoleConsumer,
		SymbolID:    "ws-b/sub.go::consume",
		RepoPrefix:  "ws-b",
		WorkspaceID: "workspace-b",
		ProjectID:   "shared",
		Meta:        map[string]any{"broker": "kafka", "topic": "user.created"},
	})

	res := Match(reg)
	if len(res.Matched) != 0 {
		t.Errorf("expected zero matched pairs (cross-workspace), got %d", len(res.Matched))
	}
	if len(res.OrphanProviders) != 1 || len(res.OrphanConsumers) != 1 {
		t.Errorf("expected 1 orphan provider + 1 orphan consumer, got %d / %d",
			len(res.OrphanProviders), len(res.OrphanConsumers))
	}
}

// TestTopicMatch_DynamicTopicNoEmission verifies the
// upstream-extractor behaviour the matcher depends on: a callsite
// whose topic argument is a variable produces no contract, so the
// matcher never sees one. This is the contract-pairer side of the
// dynamic-topic cases tested per-broker in topics_brokers_test.go.
func TestTopicMatch_DynamicTopicNoEmission(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(p *kafka.Producer, name string) {
	p.Produce(name, nil)
}

func consume(p *kafka.Producer, name string) {
	p.Subscribe(name, nil)
}
`)
	got := ext.Extract("dyn.go", src, nil, nil)
	if len(got) != 0 {
		t.Fatalf("expected 0 contracts from dynamic-topic source, got %d (%+v)",
			len(got), got)
	}

	// And confirm the matcher pipeline behaves correctly when fed
	// nothing: an empty registry yields zero matches and zero
	// orphans.
	reg := NewRegistry()
	res := Match(reg)
	if len(res.Matched) != 0 || len(res.OrphanProviders) != 0 || len(res.OrphanConsumers) != 0 {
		t.Errorf("expected an empty result, got matched=%d providers=%d consumers=%d",
			len(res.Matched), len(res.OrphanProviders), len(res.OrphanConsumers))
	}
}
