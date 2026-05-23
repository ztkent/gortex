package contracts

import (
	"testing"
)

func TestTopicExtractor_GoPublisher(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
func publish(p *kafka.Producer) {
	p.Produce("user.created", payload)
	nc.Publish("order.shipped", data)
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 2 {
		t.Fatalf("expected 2 provider contracts, got %d", len(contracts))
	}
	// Kafka Produce + NATS Publish — broker tag rides on the
	// Contract.ID so cross-broker pairing stays isolated.
	assertContract(t, contracts[0], "topic::kafka::user.created", ContractTopic, RoleProvider)
	assertContract(t, contracts[1], "topic::nats::order.shipped", ContractTopic, RoleProvider)
}

func TestTopicExtractor_GoSubscriber(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
func consume(c *kafka.Consumer) {
	c.Subscribe("user.created", nil)
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 consumer contract, got %d", len(contracts))
	}
	// `.Subscribe("subject", ...)` shape is the NATS idiom; the
	// Kafka equivalent is SubscribeTopics([]string{...}). With no
	// disambiguating import context inside the contract extractor,
	// the patterns route to NATS by default.
	assertContract(t, contracts[0], "topic::nats::user.created", ContractTopic, RoleConsumer)
}

// TestTopicExtractor_SymbolID verifies that publish/subscribe sites get
// a SymbolID pointing at the enclosing function — the prerequisite for
// EdgeMatches bridges across services (T1.2 skips any match whose
// SymbolID is empty on either side, so without this every topic pair
// would silently fail to produce a cross-service edge even when IDs
// match perfectly).
func TestTopicExtractor_SymbolID(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`package events

func publishCreated(p *kafka.Producer) {
	p.Publish("user.created", data)
}

func subscribeCreated(c *kafka.Consumer) {
	c.Subscribe("user.created", nil)
}
`)
	nodes := makeNodes("events.go", []struct {
		name       string
		start, end int
	}{
		{"publishCreated", 3, 5},
		{"subscribeCreated", 7, 9},
	})

	contracts := ext.Extract("events.go", src, nodes, nil)
	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}

	byRole := make(map[Role]Contract)
	for _, c := range contracts {
		byRole[c.Role] = c
	}

	prov, ok := byRole[RoleProvider]
	if !ok {
		t.Fatal("missing provider contract")
	}
	if prov.SymbolID != "events.go::publishCreated" {
		t.Errorf("provider SymbolID: want events.go::publishCreated, got %q", prov.SymbolID)
	}

	cons, ok := byRole[RoleConsumer]
	if !ok {
		t.Fatal("missing consumer contract")
	}
	if cons.SymbolID != "events.go::subscribeCreated" {
		t.Errorf("consumer SymbolID: want events.go::subscribeCreated, got %q", cons.SymbolID)
	}
}

func TestTopicExtractor_TSProducer(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
await producer.send({ topic: "notifications", messages: [msg] });
`)
	contracts := ext.Extract("producer.ts", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 provider contract, got %d", len(contracts))
	}
	// kafkajs producer.send({ topic: "..." }) — Kafka tag.
	assertContract(t, contracts[0], "topic::kafka::notifications", ContractTopic, RoleProvider)
}
