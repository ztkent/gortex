package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// TopicExtractor detects message-broker publish (provider) and
// subscribe (consumer) call sites for the four supported broker
// families: Kafka, RabbitMQ, NATS, and Redis pub-sub. Each detected
// site becomes a ContractTopic with Role provider/consumer and
// Meta["broker"] tagging the family. The Contract.ID encodes
// `topic::<broker>::<name>` so cross-broker isolation is automatic —
// a kafka topic named "foo" and a nats subject named "foo" hash to
// different contract IDs and never pair.
//
// Dynamic topics (variable arguments, function calls, identifiers)
// don't produce contracts: every regex requires a string literal in
// the topic / subject / channel / exchange slot. That means a call
// like `nc.Publish(subject, payload)` where `subject` is a runtime
// expression is invisible to the contract matcher — there's no
// reliable way to resolve it without dataflow, and a heuristic
// string-substitution pass would generate more false pairs than
// true ones.
type TopicExtractor struct{}

// topicPattern carries a regex + the broker family the call belongs to.
// Patterns are evaluated in order; the first match wins for a given
// callsite. The captured group at index 1 must be the topic / subject /
// channel / exchange name.
type topicPattern struct {
	re     *regexp.Regexp
	broker string
}

var (
	// Provider (publish/produce) patterns. Order matters: the most
	// specific patterns come first so a Kafka `WriteMessages` block
	// with `Topic: "x"` isn't accidentally swallowed by the broader
	// `.Publish("x"` regex when both shapes appear in the same file.
	topicPublishPatterns = []topicPattern{
		// Kafka — segmentio/kafka-go Writer.WriteMessages emits one
		// or more kafka.Message{Topic: "name", Value: ...} literals.
		// We capture every Topic-tagged struct field rather than
		// requiring a syntactic association with WriteMessages so
		// helper builders that construct kafka.Message values away
		// from the call site still produce a producer contract.
		// `(?s).*?` allows nested braces (Header arrays, Time fields)
		// before the Topic field; the lazy quantifier keeps the match
		// short so a second Message literal further down doesn't get
		// folded into the first.
		{regexp.MustCompile(`(?s)kafka\.Message\s*\{.*?Topic:\s*"([^"]+)"`), "kafka"},
		// Kafka — confluent-kafka-go Producer.Produce takes a
		// *kafka.Message whose TopicPartition.Topic is a *string.
		// The common idiom uses an inline addr-of literal:
		// `Topic: &topicName,` or `Topic: kafka.StringPtr("name")`.
		// We capture either shape.
		{regexp.MustCompile(`(?s)TopicPartition\s*:\s*kafka\.TopicPartition\s*\{.*?Topic:\s*&?"([^"]+)"`), "kafka"},
		// Kafka — sarama / kafka-go SendMessage with ProducerMessage{Topic: "..."}.
		{regexp.MustCompile(`(?s)ProducerMessage\s*\{.*?Topic:\s*"([^"]+)"`), "kafka"},
		// Kafka — kafkajs / TS producer.send({ topic: "name", ... }).
		{regexp.MustCompile(`\.send\(\s*\{\s*topic:\s*"([^"]+)"`), "kafka"},
		// Kafka — confluent-kafka-go shorthand p.Produce("topic", ...)
		// (also covers kafka-python producer.produce("topic", ...)).
		{regexp.MustCompile(`\.[Pp]roduce\(\s*"([^"]+)"`), "kafka"},

		// RabbitMQ — streadway/amqp + amqp091-go Channel.Publish and
		// PublishWithContext. Pair on the exchange (first positional
		// string). routing_key is the second positional string but
		// the matcher uses exchange identity for pairing.
		{regexp.MustCompile(`\.PublishWithContext\(\s*[^,]+,\s*"([^"]+)"`), "rabbitmq"},
		{regexp.MustCompile(`\.Publish\(\s*"([^"]+)"\s*,\s*"[^"]+"\s*,`), "rabbitmq"},
		// RabbitMQ — amqplib (Node) channel.publish(exchange, routingKey, ...)
		// and pika basic_publish(exchange, routing_key, ...).
		{regexp.MustCompile(`channel\.publish\(\s*"([^"]+)"`), "rabbitmq"},
		{regexp.MustCompile(`basic_publish\([^)]*routing_key\s*=\s*"([^"]+)"`), "rabbitmq"},
		// RabbitMQ — Node amqplib sendToQueue(queue, ...).
		{regexp.MustCompile(`\.sendToQueue\(\s*"([^"]+)"`), "rabbitmq"},

		// Redis — go-redis Client.Publish(ctx, channel, msg). ctx is
		// the first positional, so we anchor on `ctx`-style identifiers
		// preceding the channel string. The ctx-first signature is the
		// dominant go-redis idiom in v8/v9, and pinning the regex here
		// keeps NATS' positional-string `.Publish("subject", payload)`
		// out of Redis' bucket. ioredis / node-redis publish() goes
		// through the `.publish(` lowercase JS variant below.
		{regexp.MustCompile(`\.Publish\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*,\s*"([^"]+)"`), "redis"},

		// NATS — nats.go Conn.Publish(subject, data) /
		// PublishRequest(subject, reply, data) / PublishMsg(msg).
		// Subject is the first positional string. Patterns ordered
		// specific-first so PublishRequest and PublishMsg win over
		// the catchall `.Publish(` regex.
		{regexp.MustCompile(`\.PublishRequest\(\s*"([^"]+)"`), "nats"},
		{regexp.MustCompile(`\.PublishMsg\(\s*"([^"]+)"`), "nats"},
		{regexp.MustCompile(`\.Publish\(\s*"([^"]+)"`), "nats"},
		// NATS — nats.js / nats-py publish("subject", ...). Same
		// lowercase shape JS Redis would otherwise reach for; Redis
		// in the JS ecosystem uses `client.publish` which goes
		// through the channel.publish branch (RabbitMQ) anyway.
		{regexp.MustCompile(`\.publish\(\s*"([^"]+)"`), "nats"},
	}

	// Consumer (subscribe/consume) patterns. Same evaluation rules
	// as the provider table.
	topicSubscribePatterns = []topicPattern{
		// Kafka — segmentio/kafka-go Reader literal with Topic field.
		// The struct literal can contain nested braces (e.g.
		// `Brokers: []string{"..."}`) before the Topic field, so we
		// match lazily across them up to the closing `}` rather than
		// stopping at the first inner `}`.
		{regexp.MustCompile(`(?s)kafka\.ReaderConfig\s*\{.*?Topic:\s*"([^"]+)"`), "kafka"},
		// Kafka — confluent-kafka-go Consumer.SubscribeTopics with
		// a string-slice literal, and the singular Subscribe variant.
		{regexp.MustCompile(`\.SubscribeTopics\(\s*\[\]string\s*\{\s*"([^"]+)"`), "kafka"},
		// Kafka — kafkajs / TS consumer.subscribe({ topic: "name" })
		// and consumer.run({ topics: ["name"] }).
		{regexp.MustCompile(`\.subscribe\(\s*\{\s*topic:\s*"([^"]+)"`), "kafka"},
		{regexp.MustCompile(`topics:\s*\[\s*"([^"]+)"`), "kafka"},

		// RabbitMQ — streadway/amqp + amqp091-go Channel.Consume(queue, ...).
		{regexp.MustCompile(`\.Consume\(\s*"([^"]+)"`), "rabbitmq"},
		// RabbitMQ — pika channel.basic_consume(queue="name", ...).
		{regexp.MustCompile(`basic_consume\([^)]*queue\s*=\s*"([^"]+)"`), "rabbitmq"},
		// RabbitMQ — amqplib (Node) channel.consume("queue", handler).
		{regexp.MustCompile(`channel\.consume\(\s*"([^"]+)"`), "rabbitmq"},

		// Redis — go-redis Client.Subscribe / PSubscribe with
		// ctx-first positional, then the channel string. Subscribe
		// accepts variadic channels; we capture the first.
		{regexp.MustCompile(`\.PSubscribe\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*,\s*"([^"]+)"`), "redis"},
		{regexp.MustCompile(`\.Subscribe\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*,\s*"([^"]+)"`), "redis"},
		// Redis — node-redis / ioredis subscribe("channel", handler).
		{regexp.MustCompile(`\.[Pp]subscribe\(\s*"([^"]+)"\s*,[^"]*\)`), "redis"},

		// NATS — nats.go Conn.QueueSubscribe(subject, queue, handler)
		// and the plain Subscribe(subject, handler) variant.
		{regexp.MustCompile(`\.QueueSubscribe\(\s*"([^"]+)"`), "nats"},
		{regexp.MustCompile(`\.SubscribeSync\(\s*"([^"]+)"`), "nats"},
		{regexp.MustCompile(`\.ChanSubscribe\(\s*"([^"]+)"`), "nats"},
		{regexp.MustCompile(`\.Subscribe\(\s*"([^"]+)"`), "nats"},
		// NATS / Python broker-agnostic subscribe(["name"]) — falls
		// through to NATS by default; mis-tagged Kafka subscribers
		// with the bare string-list form will still pair correctly
		// because both endpoints are tagged with the same broker.
		{regexp.MustCompile(`\.subscribe\(\s*\[\s*"([^"]+)"`), "nats"},
	}
)

// SupportedLanguages reports the source languages this extractor
// recognises. Go is the primary target; TS/JS/Python patterns ride
// alongside because the regex set shares enough shape with them to
// cover the common producer/consumer idioms.
func (e *TopicExtractor) SupportedLanguages() []string {
	return []string{"go", "typescript", "javascript", "python"}
}

// topicPrefilterMarkers covers every publish/subscribe regex without
// redundancy: `basic_publish` / `basic_consume` are omitted because
// `publish` / `consume` are substrings; `topic` (no colon) covers both
// `topic:` and `topics:`. A file without any of these markers cannot
// produce a topic contract, so we short-circuit before the per-broker
// FindAllStringSubmatchIndex passes.
var topicPrefilterMarkers = [][]byte{
	[]byte("Publish"),       // .Publish( / .PublishWithContext( / channel.publish(
	[]byte("publish"),       // lowercase + basic_publish + .psubscribe
	[]byte("Produce"),       // .Produce(
	[]byte("produce"),       // .produce(
	[]byte("Subscribe"),     // .Subscribe( / .SubscribeTopics( / .QueueSubscribe(
	[]byte("subscribe"),     // .subscribe( / .psubscribe(
	[]byte("Consume"),       // .Consume( (RabbitMQ Go)
	[]byte("consume"),       // .consume( / basic_consume
	[]byte("topic"),         // TS {topic:...} / {topics:[...]} / Kafka struct field
	[]byte("ReaderConfig"),  // kafka-go Reader{Topic:...}
	[]byte("WriteMessages"), // kafka-go Writer.WriteMessages
	[]byte("sendToQueue"),   // amqplib
	[]byte(".send("),        // kafkajs producer.send
}

// Extract scans src for every recognised provider / consumer call
// site and emits one ContractTopic per match. The Contract.ID is
// `topic::<broker>::<name>` so the matcher's bucket key keeps
// kafka:foo and nats:foo apart. Meta["broker"] and Meta["topic"]
// carry the structured fields the pairing pass reads.
func (e *TopicExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	if !srcHasAnyMarker(src, topicPrefilterMarkers) {
		return nil
	}

	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})

	// Track (line, role) pairs we've already emitted so two patterns
	// that both fire on the same callsite — common when a Kafka
	// WriteMessages call sits next to a kafka.Message{Topic: "x"}
	// literal — don't double-count. The first matching pattern wins;
	// the later one is suppressed for that role on that line.
	type seenKey struct {
		line int
		role Role
	}
	seen := make(map[seenKey]struct{})

	for _, pat := range topicPublishPatterns {
		for _, m := range pat.re.FindAllStringSubmatchIndex(text, -1) {
			topic := text[m[2]:m[3]]
			if topic == "" {
				continue
			}
			ln := lineNumber(lines, m[0])
			key := seenKey{ln, RoleProvider}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			contracts = append(contracts, Contract{
				ID:       fmt.Sprintf("topic::%s::%s", pat.broker, topic),
				Type:     ContractTopic,
				Role:     RoleProvider,
				SymbolID: findEnclosingSymbol(fileNodes, ln),
				FilePath: filePath,
				Line:     ln,
				Meta: map[string]any{
					"topic":  topic,
					"broker": pat.broker,
				},
				Confidence: 0.85,
			})
		}
	}

	for _, pat := range topicSubscribePatterns {
		for _, m := range pat.re.FindAllStringSubmatchIndex(text, -1) {
			topic := text[m[2]:m[3]]
			if topic == "" {
				continue
			}
			ln := lineNumber(lines, m[0])
			key := seenKey{ln, RoleConsumer}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			contracts = append(contracts, Contract{
				ID:       fmt.Sprintf("topic::%s::%s", pat.broker, topic),
				Type:     ContractTopic,
				Role:     RoleConsumer,
				SymbolID: findEnclosingSymbol(fileNodes, ln),
				FilePath: filePath,
				Line:     ln,
				Meta: map[string]any{
					"topic":  topic,
					"broker": pat.broker,
				},
				Confidence: 0.85,
			})
		}
	}

	return contracts
}
