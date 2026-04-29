package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Fixtures for the prefilter benchmarks. Each represents a "cold"
// file — typical source that the extractor should skip on
// its own, not a file containing the contract pattern. The speedup
// ratio from the prefilter only surfaces on these cold files, and
// cold files dominate a mixed-language repo.

var (
	benchColdTSSrc = []byte(`
import { describe, it, expect } from 'vitest';
import { foo, bar, baz } from './utils';

describe('suite', () => {
  it('does a thing', () => {
    const result = foo(bar(baz()));
    expect(result).toBe(42);
  });

  it('handles edge cases', () => {
    const items = [1, 2, 3].map(x => x * 2);
    expect(items.length).toBe(3);
  });
});

export class Helper {
  constructor(private name: string) {}
  greet() { return ` + "`" + `hello ${this.name}` + "`" + `; }
}
`)

	benchColdGoSrc = []byte(`
package utils

import (
	"fmt"
	"strings"
)

type Formatter struct {
	prefix string
}

func NewFormatter(prefix string) *Formatter {
	return &Formatter{prefix: prefix}
}

func (f *Formatter) Format(parts ...string) string {
	return f.prefix + strings.Join(parts, "-")
}

func Reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return fmt.Sprint(string(runes))
}
`)

	benchColdPySrc = []byte(`
from dataclasses import dataclass
from typing import List, Optional

@dataclass
class Item:
    id: int
    name: str
    tags: List[str]

def find_items(items: List[Item], tag: str) -> List[Item]:
    return [i for i in items if tag in i.tags]

def first_or_none(items: List[Item]) -> Optional[Item]:
    return items[0] if items else None
`)
)

func BenchmarkHTTPExtractor_ColdTS(b *testing.B) {
	ext := &HTTPExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("helper.ts", benchColdTSSrc, nodes, nil)
	}
}

func BenchmarkHTTPExtractor_ColdGo(b *testing.B) {
	ext := &HTTPExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("utils.go", benchColdGoSrc, nodes, nil)
	}
}

func BenchmarkGraphQLExtractor_ColdTS(b *testing.B) {
	ext := &GraphQLExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("helper.ts", benchColdTSSrc, nodes, nil)
	}
}

func BenchmarkTopicExtractor_ColdTS(b *testing.B) {
	ext := &TopicExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("helper.ts", benchColdTSSrc, nodes, nil)
	}
}

func BenchmarkWebSocketExtractor_ColdTS(b *testing.B) {
	ext := &WebSocketExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("helper.ts", benchColdTSSrc, nodes, nil)
	}
}

func BenchmarkEnvVarExtractor_ColdPy(b *testing.B) {
	ext := &EnvVarExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("model.py", benchColdPySrc, nodes, nil)
	}
}

func BenchmarkEnvVarExtractor_ColdGo(b *testing.B) {
	ext := &EnvVarExtractor{}
	nodes := []*graph.Node{}
	for b.Loop() {
		_ = ext.Extract("utils.go", benchColdGoSrc, nodes, nil)
	}
}
