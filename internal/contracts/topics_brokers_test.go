package contracts

import "testing"

// Per-broker producer / consumer detection coverage for the four
// supported message-broker families: Kafka, RabbitMQ, NATS, Redis.
// Each broker has a producer test, a consumer test, and a dynamic-
// topic suppression test. The contract-pairing tests live in
// topics_pairing_test.go.

// -- Kafka --------------------------------------------------------

func TestTopicExtractor_Kafka_Producer_KafkaGoWriter(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

import "github.com/segmentio/kafka-go"

func publish(w *kafka.Writer) {
	w.WriteMessages(ctx, kafka.Message{Topic: "orders.created", Value: payload})
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 kafka producer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "kafka", "orders.created", RoleProvider)
}

func TestTopicExtractor_Kafka_Producer_ConfluentProduce(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(p *kafka.Producer) {
	p.Produce("user.created", payload)
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 kafka producer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "kafka", "user.created", RoleProvider)
}

func TestTopicExtractor_Kafka_Consumer_SubscribeTopics(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(c *kafka.Consumer) {
	c.SubscribeTopics([]string{"user.created"}, nil)
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 kafka consumer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "kafka", "user.created", RoleConsumer)
}

func TestTopicExtractor_Kafka_Consumer_ReaderConfig(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

import "github.com/segmentio/kafka-go"

func make() *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{"localhost:9092"},
		Topic:   "events.audit",
	})
}
`)
	contracts := ext.Extract("reader.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 kafka reader-config consumer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "kafka", "events.audit", RoleConsumer)
}

// Dynamic topic — passed as a variable, not a literal. The string-
// literal regex shouldn't fire; the contract pairer never sees it.
func TestTopicExtractor_Kafka_DynamicTopic_NotEmitted(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(p *kafka.Producer, topic string) {
	p.Produce(topic, payload)
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 0 {
		t.Fatalf("expected 0 contracts for dynamic topic, got %d (%+v)", len(contracts), contracts)
	}
}

// -- RabbitMQ -----------------------------------------------------

func TestTopicExtractor_RabbitMQ_Producer_PublishWithContext(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

import amqp "github.com/rabbitmq/amqp091-go"

func publish(ch *amqp.Channel) {
	ch.PublishWithContext(ctx, "user.events", "user.created", false, false, amqp.Publishing{Body: payload})
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 rabbitmq producer contract, got %d (%+v)", len(contracts), contracts)
	}
	// Exchange is the pairing identity; routing key is metadata.
	assertBroker(t, contracts[0], "rabbitmq", "user.events", RoleProvider)
}

func TestTopicExtractor_RabbitMQ_Producer_PublishExchange(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(ch *amqp.Channel) {
	ch.Publish("orders", "order.placed", false, false, amqp.Publishing{Body: payload})
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 rabbitmq producer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "rabbitmq", "orders", RoleProvider)
}

func TestTopicExtractor_RabbitMQ_Consumer_Consume(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(ch *amqp.Channel) {
	msgs, _ := ch.Consume("order-events", "consumer-1", true, false, false, false, nil)
	_ = msgs
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 rabbitmq consumer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "rabbitmq", "order-events", RoleConsumer)
}

func TestTopicExtractor_RabbitMQ_DynamicTopic_NotEmitted(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(ch *amqp.Channel, queue string) {
	ch.Consume(queue, "consumer-1", true, false, false, false, nil)
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 0 {
		t.Fatalf("expected 0 contracts for dynamic queue, got %d (%+v)", len(contracts), contracts)
	}
}

// -- NATS ---------------------------------------------------------

func TestTopicExtractor_NATS_Producer_Publish(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

import "github.com/nats-io/nats.go"

func publish(nc *nats.Conn) {
	nc.Publish("orders.created", []byte("payload"))
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 nats producer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "nats", "orders.created", RoleProvider)
}

func TestTopicExtractor_NATS_Producer_PublishRequest(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func ask(nc *nats.Conn) {
	nc.PublishRequest("orders.created", "reply.box", payload)
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 nats publish-request contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "nats", "orders.created", RoleProvider)
}

func TestTopicExtractor_NATS_Consumer_Subscribe(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(nc *nats.Conn) {
	nc.Subscribe("orders.created", handler)
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 nats consumer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "nats", "orders.created", RoleConsumer)
}

func TestTopicExtractor_NATS_Consumer_QueueSubscribe(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(nc *nats.Conn) {
	nc.QueueSubscribe("orders.created", "workers", handler)
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 nats queue-subscribe contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "nats", "orders.created", RoleConsumer)
}

func TestTopicExtractor_NATS_DynamicTopic_NotEmitted(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(nc *nats.Conn, subject string) {
	nc.Publish(subject, payload)
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 0 {
		t.Fatalf("expected 0 contracts for dynamic subject, got %d (%+v)", len(contracts), contracts)
	}
}

// -- Redis pub/sub ------------------------------------------------

func TestTopicExtractor_Redis_Producer_Publish(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

import "github.com/redis/go-redis/v9"

func publish(rdb *redis.Client) {
	rdb.Publish(ctx, "channel.users", "payload")
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 redis producer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "redis", "channel.users", RoleProvider)
}

func TestTopicExtractor_Redis_Consumer_Subscribe(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(rdb *redis.Client) {
	rdb.Subscribe(ctx, "channel.users")
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 redis consumer contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "redis", "channel.users", RoleConsumer)
}

func TestTopicExtractor_Redis_Consumer_PSubscribe(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func consume(rdb *redis.Client) {
	rdb.PSubscribe(ctx, "channel.*")
}
`)
	contracts := ext.Extract("consumer.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 redis psubscribe contract, got %d (%+v)", len(contracts), contracts)
	}
	assertBroker(t, contracts[0], "redis", "channel.*", RoleConsumer)
}

func TestTopicExtractor_Redis_DynamicTopic_NotEmitted(t *testing.T) {
	ext := &TopicExtractor{}
	src := []byte(`
package main

func publish(rdb *redis.Client, ch string) {
	rdb.Publish(ctx, ch, "payload")
}
`)
	contracts := ext.Extract("publisher.go", src, nil, nil)
	if len(contracts) != 0 {
		t.Fatalf("expected 0 contracts for dynamic channel, got %d (%+v)", len(contracts), contracts)
	}
}

// assertBroker is a topic-specific assertion helper. It checks the
// contract's role, ID shape, broker meta, and raw topic name in one
// call so per-broker tests stay terse.
func assertBroker(t *testing.T, c Contract, broker, topic string, role Role) {
	t.Helper()
	wantID := "topic::" + broker + "::" + topic
	if c.ID != wantID {
		t.Errorf("contract ID: want %q, got %q", wantID, c.ID)
	}
	if c.Type != ContractTopic {
		t.Errorf("contract Type: want %q, got %q", ContractTopic, c.Type)
	}
	if c.Role != role {
		t.Errorf("contract Role: want %q, got %q", role, c.Role)
	}
	if got, _ := c.Meta["broker"].(string); got != broker {
		t.Errorf("Meta[broker]: want %q, got %q", broker, got)
	}
	if got, _ := c.Meta["topic"].(string); got != topic {
		t.Errorf("Meta[topic]: want %q, got %q", topic, got)
	}
}
