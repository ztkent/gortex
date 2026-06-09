package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	traceIndex             string
	traceDepth             int
	traceK                 int
	traceMaxFrontier       int
	traceMinTier           string
	traceIncludeReferences bool
	traceFormat            string
)

var traceCmd = &cobra.Command{
	Use:   "trace <from-id> <to-id>",
	Short: "Trace the shortest call path between two symbols",
	Long: `Trace the shortest call path from one symbol to another over the call graph
(calls + cross-service matches + method-value references).

When no path exists, trace prints a why-unreachable diagnosis: the functions
reachable from the source, the functions that can reach the sink, and the
dynamic-dispatch / external boundaries where the chain breaks — so you know
whether the two are genuinely disconnected or merely separated by an
unresolved interface the graph could not bind.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		toolArgs := map[string]any{
			"source_id":          args[0],
			"sink_id":            args[1],
			"max_depth":          traceDepth,
			"k":                  traceK,
			"max_frontier":       traceMaxFrontier,
			"include_references": traceIncludeReferences,
		}
		if traceMinTier != "" {
			toolArgs["min_tier"] = traceMinTier
		}
		out, err := requireDaemonTool(traceIndex, "trace_path", toolArgs)
		if err != nil {
			return err
		}
		return printDaemonTracePath(cmd, out)
	},
}

func init() {
	traceCmd.Flags().StringVar(&traceIndex, "index", ".", "repository path the daemon must track")
	traceCmd.Flags().IntVar(&traceDepth, "depth", 24, "maximum combined search depth")
	traceCmd.Flags().IntVar(&traceK, "k", 1, "number of distinct shortest paths to return")
	traceCmd.Flags().IntVar(&traceMaxFrontier, "max-frontier", 25, "cap on frontier nodes in the gap report")
	traceCmd.Flags().StringVar(&traceMinTier, "min-tier", "", "minimum per-edge provenance tier to traverse (lsp_resolved|ast_resolved|...)")
	traceCmd.Flags().BoolVar(&traceIncludeReferences, "include-references", true, "traverse method-value wiring edges (mux.HandleFunc, command tables)")
	traceCmd.Flags().StringVar(&traceFormat, "format", "text", "output format: text|json")
	rootCmd.AddCommand(traceCmd)
}

type traceFrontierNode struct {
	ID    string `json:"id"`
	Depth int    `json:"depth"`
}

// printDaemonTracePath renders trace_path: the path as a hop list for the
// found case, and the full why-unreachable diagnosis otherwise.
func printDaemonTracePath(cmd *cobra.Command, raw json.RawMessage) error {
	if traceFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		Found  bool   `json:"found"`
		SrcID  string `json:"source_id"`
		SinkID string `json:"sink_id"`
		Paths  []struct {
			Nodes      []string `json:"nodes"`
			Length     int      `json:"length"`
			Confidence float64  `json:"confidence"`
			WorstTier  string   `json:"worst_tier"`
			Edges      []struct {
				To   string `json:"to"`
				Kind string `json:"kind"`
				Tier string `json:"tier"`
			} `json:"edges"`
		} `json:"paths"`
		Gap *struct {
			Reason             string              `json:"reason"`
			Message            string              `json:"message"`
			ForwardReached     int                 `json:"forward_reached"`
			BackwardReached    int                 `json:"backward_reached"`
			FurthestFromSource []traceFrontierNode `json:"furthest_from_source"`
			NearestToSink      []traceFrontierNode `json:"nearest_to_sink"`
			BoundaryHits       []struct {
				From     string `json:"from"`
				Target   string `json:"target"`
				Reason   string `json:"reason"`
				EdgeKind string `json:"edge_kind"`
			} `json:"boundary_hits"`
		} `json:"gap"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	out := cmd.OutOrStdout()
	if payload.Found {
		for i, p := range payload.Paths {
			if len(payload.Paths) > 1 {
				_, _ = fmt.Fprintf(out, "Path %d (length %d, confidence %.2f, %s):\n", i+1, p.Length, p.Confidence, p.WorstTier)
			}
			if len(p.Nodes) > 0 {
				_, _ = fmt.Fprintf(out, "  %s\n", p.Nodes[0])
				for _, e := range p.Edges {
					_, _ = fmt.Fprintf(out, "    --%s(%s)--> %s\n", e.Kind, e.Tier, e.To)
				}
			}
		}
		return nil
	}
	if payload.Gap == nil {
		_, _ = fmt.Fprintln(out, "no path found")
		return nil
	}
	g := payload.Gap
	_, _ = fmt.Fprintf(out, "No call path from %s to %s.\n", payload.SrcID, payload.SinkID)
	_, _ = fmt.Fprintf(out, "Reason: %s\n", g.Reason)
	if g.Message != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", g.Message)
	}
	_, _ = fmt.Fprintf(out, "Forward reach: %d   Backward reach: %d\n", g.ForwardReached, g.BackwardReached)
	if len(g.FurthestFromSource) > 0 {
		_, _ = fmt.Fprintln(out, "Furthest reachable from source:")
		for _, n := range g.FurthestFromSource {
			_, _ = fmt.Fprintf(out, "  %s (depth %d)\n", n.ID, n.Depth)
		}
	}
	if len(g.NearestToSink) > 0 {
		_, _ = fmt.Fprintln(out, "Nearest that reach the sink:")
		for _, n := range g.NearestToSink {
			_, _ = fmt.Fprintf(out, "  %s (depth %d)\n", n.ID, n.Depth)
		}
	}
	if len(g.BoundaryHits) > 0 {
		_, _ = fmt.Fprintln(out, "Boundaries where the chain breaks:")
		for _, b := range g.BoundaryHits {
			_, _ = fmt.Fprintf(out, "  %s -> %s [%s via %s]\n", b.From, b.Target, b.Reason, b.EdgeKind)
		}
	}
	return nil
}
