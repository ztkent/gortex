package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// EventBusExtractor synthesizes provider/consumer contracts for a bespoke
// event bus declared entirely in config — no code change, no per-language
// parser work. It is the config-driven counterpart to the hard-coded
// TopicExtractor: each user boundary matches call sites / decorators /
// interface bases (language-agnostically, via regex over the source the same
// way httpPatterns / topicPublishPatterns do) and emits a ContractTopic keyed
// `topic::<bus>::<topic>`. The existing contract matcher then pairs a producer
// and consumer of the same bus+topic into a traversable EdgeMatches edge —
// strictly better than pygraph/tsgraph, which only stash isolated metadata on
// each symbol and never build the producer→consumer link.
//
// Dispatch guards (the first if/elif chain of a consumer body) are extracted
// best-effort onto the contract's Meta["guards"] so analyze can answer
// "which handler dispatches on entity==ORDER".
type EventBusExtractor struct {
	Boundaries []EventBusBoundary
}

// EventBusBoundary is one declarative producer/consumer rule. It mirrors
// config.EventBusBoundarySpec but lives in the contracts package so the
// resolver/contracts layers never import internal/config (cycle-free); the
// indexer converts specs to boundaries at construction.
type EventBusBoundary struct {
	Name          string
	Type          string // "producer" | "consumer"
	Callee        string // dotted call suffix (producer/consumer call)
	CalleePattern string // substring match on the callee (SSE / hook)
	Decorator     string // decorator name (consumer)
	Interface     string // class base name (consumer)
	TopicArg      string // positional index ("0") or kwarg name; default "topic"
	Guards        bool   // extract dispatch guards from the consumer body
}

// eventBusLangs are the languages the config-driven bus runs over. It covers
// the competitors' targets (Python, TS/JS) plus the other common backend
// languages so one config rule works across a polyglot monorepo.
var eventBusLangs = []string{
	"python", "javascript", "typescript", "go", "java", "ruby", "php", "rust", "csharp", "kotlin",
}

func (e *EventBusExtractor) SupportedLanguages() []string { return eventBusLangs }

func (e *EventBusExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	if len(e.Boundaries) == 0 {
		return nil
	}
	text := string(src)
	lines := strings.Split(text, "\n")
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool { return fileNodes[i].StartLine < fileNodes[j].StartLine })

	var out []Contract
	for _, b := range e.Boundaries {
		switch {
		case b.Decorator != "":
			out = append(out, e.matchDecorator(b, filePath, text, lines, fileNodes)...)
		case b.Interface != "":
			out = append(out, e.matchInterface(b, filePath, text, lines, fileNodes)...)
		default:
			out = append(out, e.matchCallee(b, filePath, text, lines, fileNodes)...)
		}
	}
	return out
}

func (e *EventBusExtractor) role(b EventBusBoundary) Role {
	if strings.EqualFold(b.Type, "producer") {
		return RoleProvider
	}
	return RoleConsumer
}

// matchCallee handles producer calls (boundary.Callee) and SSE/hook consumer
// calls (boundary.CalleePattern / HookPattern).
func (e *EventBusExtractor) matchCallee(b EventBusBoundary, filePath, text string, lines []string, fileNodes []*graph.Node) []Contract {
	var re *regexp.Regexp
	switch {
	case b.Callee != "":
		// Substring match on the dotted callee so "self.producer.send(" matches "producer.send".
		re = regexp.MustCompile(regexp.QuoteMeta(b.Callee) + `\s*\(`)
	case b.CalleePattern != "":
		re = regexp.MustCompile(regexp.QuoteMeta(b.CalleePattern) + `[\w.]*\s*\(`)
	default:
		return nil
	}
	var out []Contract
	for _, m := range re.FindAllStringIndex(text, -1) {
		args := callTrailSlice(text, m[0])
		topic := extractTopicArg(splitPyParams(args), b.TopicArg)
		if topic == "" {
			continue
		}
		ln := lineAtOffset(lines, m[0])
		c := e.contractFor(b, topic, findEnclosingSymbol(fileNodes, ln), filePath, ln)
		if b.Guards && e.role(b) == RoleConsumer {
			attachGuards(&c, lines, fileNodes, c.SymbolID)
		}
		out = append(out, c)
	}
	return out
}

// matchDecorator handles decorator-based consumers (@kafka_consumer(topic=...)).
func (e *EventBusExtractor) matchDecorator(b EventBusBoundary, filePath, text string, lines []string, fileNodes []*graph.Node) []Contract {
	re := regexp.MustCompile(`@` + regexp.QuoteMeta(b.Decorator) + `\b`)
	var out []Contract
	for _, m := range re.FindAllStringIndex(text, -1) {
		ln := lineAtOffset(lines, m[0])
		// The decorated function is the next function/method node at or after
		// the decorator line.
		sym := decoratedSymbol(fileNodes, ln)
		if sym == "" {
			sym = findEnclosingSymbol(fileNodes, ln)
		}
		// Topic from the decorator's args, when it is a call form.
		topic := ""
		if args := callTrailSlice(text, m[0]); args != "" {
			topic = extractTopicArg(splitPyParams(args), b.TopicArg)
		}
		if topic == "" {
			topic = "*"
		}
		c := e.contractFor(b, topic, sym, filePath, ln)
		if b.Guards && e.role(b) == RoleConsumer {
			attachGuards(&c, lines, fileNodes, sym)
		}
		out = append(out, c)
	}
	return out
}

// matchInterface handles interface-based consumers: every method of a class
// that extends the named base is a consumer, paired on the bus wildcard topic.
func (e *EventBusExtractor) matchInterface(b EventBusBoundary, filePath, text string, lines []string, fileNodes []*graph.Node) []Contract {
	classRE := regexp.MustCompile(`class\s+(\w+)\s*[\(:][^\n{]*\b` + regexp.QuoteMeta(b.Interface) + `\b`)
	var out []Contract
	seen := map[string]bool{}
	for _, m := range classRE.FindAllStringSubmatch(text, -1) {
		className := m[1]
		for _, n := range fileNodes {
			if n.Kind != graph.KindMethod {
				continue
			}
			recv, _ := n.Meta["receiver"].(string)
			if recv != className && !strings.HasPrefix(n.ID, filePath+"::"+className+".") {
				continue
			}
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			c := e.contractFor(b, "*", n.ID, filePath, n.StartLine)
			if b.Guards && e.role(b) == RoleConsumer {
				attachGuards(&c, lines, fileNodes, n.ID)
			}
			out = append(out, c)
		}
	}
	return out
}

func (e *EventBusExtractor) contractFor(b EventBusBoundary, topic, symbolID, filePath string, line int) Contract {
	meta := map[string]any{
		"topic":     topic,
		"bus":       b.Name,
		"transport": "configbus",
	}
	return Contract{
		ID:         fmt.Sprintf("topic::%s::%s", b.Name, topic),
		Type:       ContractTopic,
		Role:       e.role(b),
		SymbolID:   symbolID,
		FilePath:   filePath,
		Line:       line,
		Meta:       meta,
		Confidence: 0.7,
	}
}

// extractTopicArg pulls the topic/url literal from a parsed arg list by
// positional index (TopicArg is numeric) or kwarg name, falling back to the
// first positional string literal.
func extractTopicArg(parts []string, topicArg string) string {
	if topicArg == "" {
		topicArg = "topic"
	}
	if idx, err := strconv.Atoi(topicArg); err == nil {
		if idx >= 0 && idx < len(parts) {
			if lit, ok := pyStringLiteral(parts[idx]); ok {
				return lit
			}
		}
		return firstPositionalString(parts)
	}
	for _, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(p[:eq]) == topicArg {
			if lit, ok := pyStringLiteral(p[eq+1:]); ok {
				return lit
			}
		}
	}
	return firstPositionalString(parts)
}

func firstPositionalString(parts []string) string {
	for _, p := range parts {
		if strings.Contains(p, "=") {
			continue
		}
		if lit, ok := pyStringLiteral(p); ok {
			return lit
		}
	}
	return ""
}

// decoratedSymbol returns the ID of the first function/method node at or after
// the decorator line.
func decoratedSymbol(fileNodes []*graph.Node, decoratorLine int) string {
	best := ""
	bestLine := 1 << 30
	for _, n := range fileNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine >= decoratorLine && n.StartLine < bestLine {
			bestLine = n.StartLine
			best = n.ID
		}
	}
	return best
}

var (
	guardEqRE  = regexp.MustCompile(`(?:if|elif)\s+(.+?)\s*==\s*['"]([^'"]+)['"]`)
	guardGetRE = regexp.MustCompile(`\w+\.get\(\s*['"]([^'"]+)['"]`)
)

// attachGuards scans the consumer symbol's body for the first if/elif dispatch
// chain (field==value rules) and records them on the contract's Meta["guards"].
func attachGuards(c *Contract, lines []string, fileNodes []*graph.Node, symbolID string) {
	start, end := symbolLineRange(fileNodes, symbolID)
	if start == 0 || end < start {
		return
	}
	var guards []map[string]string
	for i := start - 1; i < end && i < len(lines); i++ {
		for _, mm := range guardEqRE.FindAllStringSubmatch(lines[i], -1) {
			field := strings.TrimSpace(mm[1])
			if g := guardGetRE.FindStringSubmatch(field); len(g) == 2 {
				field = g[1]
			}
			guards = append(guards, map[string]string{"field": field, "value": mm[2]})
		}
	}
	if len(guards) > 0 {
		c.Meta["guards"] = guards
	}
}

func symbolLineRange(fileNodes []*graph.Node, symbolID string) (int, int) {
	for _, n := range fileNodes {
		if n.ID == symbolID {
			return n.StartLine, n.EndLine
		}
	}
	return 0, 0
}
