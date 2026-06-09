package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func ebFind(cs []Contract, id string, role Role) *Contract {
	for i := range cs {
		if cs[i].ID == id && cs[i].Role == role {
			return &cs[i]
		}
	}
	return nil
}

func TestEventBus_ProducerCallee(t *testing.T) {
	src := []byte(`def emit():
    producer.send(topic='orders')
`)
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "kafka", Type: "producer", Callee: "producer.send", TopicArg: "topic"},
	}}
	cs := ext.Extract("app.py", src, nil, nil)
	c := ebFind(cs, "topic::kafka::orders", RoleProvider)
	if c == nil {
		t.Fatalf("expected provider topic::kafka::orders, got %+v", cs)
	}
	if c.Type != ContractTopic || c.Meta["topic"] != "orders" {
		t.Errorf("unexpected contract: %+v", c)
	}
}

func TestEventBus_SSEConsumerPattern(t *testing.T) {
	src := []byte(`const es = new EventSource('/api/events')`)
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "sse", Type: "consumer", CalleePattern: "EventSource", TopicArg: "0"},
	}}
	cs := ext.Extract("app.ts", src, nil, nil)
	if ebFind(cs, "topic::sse::/api/events", RoleConsumer) == nil {
		t.Fatalf("expected SSE consumer on /api/events, got %+v", cs)
	}
}

func TestEventBus_DecoratorConsumer(t *testing.T) {
	src := []byte(`@kafka_consumer(topic='orders')
def handle_order(msg):
    pass
`)
	nodes := []*graph.Node{
		{ID: "app.py::handle_order", Kind: graph.KindFunction, Name: "handle_order", FilePath: "app.py", StartLine: 2, EndLine: 3},
	}
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "kafka", Type: "consumer", Decorator: "kafka_consumer", TopicArg: "topic"},
	}}
	cs := ext.Extract("app.py", src, nodes, nil)
	c := ebFind(cs, "topic::kafka::orders", RoleConsumer)
	if c == nil {
		t.Fatalf("expected decorator consumer, got %+v", cs)
	}
	if c.SymbolID != "app.py::handle_order" {
		t.Errorf("SymbolID = %q, want the decorated function", c.SymbolID)
	}
}

func TestEventBus_InterfaceConsumer(t *testing.T) {
	src := []byte(`class OrderHandler(CallbackHandler):
    def handle(self):
        pass
`)
	nodes := []*graph.Node{
		{ID: "app.py::OrderHandler", Kind: graph.KindType, Name: "OrderHandler", FilePath: "app.py", StartLine: 1, EndLine: 3},
		{ID: "app.py::OrderHandler.handle", Kind: graph.KindMethod, Name: "handle", FilePath: "app.py", StartLine: 2, EndLine: 3, Meta: map[string]any{"receiver": "OrderHandler"}},
	}
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "bus", Type: "consumer", Interface: "CallbackHandler"},
	}}
	cs := ext.Extract("app.py", src, nodes, nil)
	c := ebFind(cs, "topic::bus::*", RoleConsumer)
	if c == nil {
		t.Fatalf("expected interface consumer on bus wildcard, got %+v", cs)
	}
	if c.SymbolID != "app.py::OrderHandler.handle" {
		t.Errorf("SymbolID = %q, want the method node", c.SymbolID)
	}
}

func TestEventBus_DispatchGuards(t *testing.T) {
	src := []byte(`def dispatch(data):
    if data.get('entity') == 'ORDER':
        do_order()
    elif command == 'CREATE':
        do_create()
`)
	nodes := []*graph.Node{
		{ID: "app.py::dispatch", Kind: graph.KindFunction, Name: "dispatch", FilePath: "app.py", StartLine: 1, EndLine: 5},
	}
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "cmd", Type: "consumer", Decorator: "command_handler", TopicArg: "topic", Guards: true},
	}}
	src2 := append([]byte("@command_handler(topic='cmd')\n"), src...)
	// Shift node lines by 1 for the added decorator line.
	nodes[0].StartLine = 2
	nodes[0].EndLine = 6
	cs := ext.Extract("app.py", src2, nodes, nil)
	c := ebFind(cs, "topic::cmd::cmd", RoleConsumer)
	if c == nil {
		t.Fatalf("expected consumer, got %+v", cs)
	}
	guards, _ := c.Meta["guards"].([]map[string]string)
	if len(guards) != 2 {
		t.Fatalf("expected 2 dispatch guards, got %+v", c.Meta["guards"])
	}
	if guards[0]["field"] != "entity" || guards[0]["value"] != "ORDER" {
		t.Errorf("guard[0] = %v, want entity==ORDER", guards[0])
	}
	if guards[1]["field"] != "command" || guards[1]["value"] != "CREATE" {
		t.Errorf("guard[1] = %v, want command==CREATE", guards[1])
	}
}

func TestEventBus_TopicArgPositional(t *testing.T) {
	src := []byte(`bus.publish("events", payload)`)
	ext := &EventBusExtractor{Boundaries: []EventBusBoundary{
		{Name: "b", Type: "producer", Callee: "bus.publish", TopicArg: "0"},
	}}
	cs := ext.Extract("a.go", src, nil, nil)
	if ebFind(cs, "topic::b::events", RoleProvider) == nil {
		t.Fatalf("expected positional topic extraction, got %+v", cs)
	}
}

func TestEventBus_EmptyBoundaries(t *testing.T) {
	cs := (&EventBusExtractor{}).Extract("a.py", []byte(`producer.send(topic='x')`), nil, nil)
	if len(cs) != 0 {
		t.Errorf("expected no contracts with no boundaries, got %+v", cs)
	}
}
