package docs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RenderMarkdown projects a Bundle to markdown. Section order follows
// Bundle.Sections.
func RenderMarkdown(b *Bundle) string {
	if b == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Docs Bundle\n\n")
	fmt.Fprintf(&sb, "Generated %s. Window since %s.\n\n",
		b.GeneratedAt.Format(time.RFC3339), b.Since.Format(time.RFC3339))

	for _, section := range b.Sections {
		switch strings.ToLower(section) {
		case "recent":
			renderRecent(&sb, b.RecentChanges)
		case "ownership":
			renderOwnership(&sb, b.OwnershipRows)
		case "stale":
			renderStale(&sb, b.StaleCodeRows)
		case "blame":
			renderBlame(&sb, b.Blame)
		}
	}
	return sb.String()
}

// RenderJSON marshals a Bundle to indented JSON.
func RenderJSON(b *Bundle) ([]byte, error) {
	return json.MarshalIndent(b, "", "  ")
}

func renderRecent(sb *strings.Builder, evs []HistoryEvent) {
	sb.WriteString("## Recent Changes\n\n")
	if len(evs) == 0 {
		sb.WriteString("_No file changes recorded in this window. (Was the watcher attached?)_\n\n")
		return
	}
	sb.WriteString("| Timestamp | Kind | File | +Nodes | -Nodes |\n")
	sb.WriteString("|-----------|------|------|--------|--------|\n")
	for _, ev := range evs {
		fmt.Fprintf(sb, "| %s | %s | `%s` | %d | %d |\n",
			ev.Timestamp.Format(time.RFC3339), ev.Kind, ev.FilePath,
			ev.NodesAdded, ev.NodesRemoved)
	}
	sb.WriteString("\n")
}

func renderOwnership(sb *strings.Builder, rows []OwnershipRow) {
	sb.WriteString("## Ownership\n\n")
	if len(rows) == 0 {
		sb.WriteString("_No blame metadata on graph nodes. Run `gortex enrich blame` or `analyze kind:blame` first._\n\n")
		return
	}
	sb.WriteString("| Author | Symbols | Files | Oldest | Newest |\n")
	sb.WriteString("|--------|---------|-------|--------|--------|\n")
	for _, r := range rows {
		fmt.Fprintf(sb, "| %s | %d | %d | %s | %s |\n",
			r.Email, r.Symbols, r.Files,
			formatTimestamp(r.Oldest), formatTimestamp(r.Newest))
	}
	sb.WriteString("\n")
}

func renderStale(sb *strings.Builder, rows []StaleCodeRow) {
	sb.WriteString("## Stale Code\n\n")
	if len(rows) == 0 {
		sb.WriteString("_No stale code older than 365 days, or blame metadata missing._\n\n")
		return
	}
	sb.WriteString("| Age (days) | Symbol | Author | Commit |\n")
	sb.WriteString("|------------|--------|--------|--------|\n")
	for _, r := range rows {
		commitShort := r.Commit
		if len(commitShort) > 8 {
			commitShort = commitShort[:8]
		}
		sym := r.ID
		if r.File != "" {
			sym = fmt.Sprintf("`%s` (%s:%d)", r.ID, r.File, r.Line)
		}
		fmt.Fprintf(sb, "| %d | %s | %s | `%s` |\n",
			r.AgeDays, sym, r.Email, commitShort)
	}
	sb.WriteString("\n")
}

func renderBlame(sb *strings.Builder, sum *BlameSummary) {
	sb.WriteString("## Blame Enrichment\n\n")
	if sum == nil {
		sb.WriteString("_Blame re-run was not requested in this bundle._\n\n")
		return
	}
	if sum.Error != "" {
		fmt.Fprintf(sb, "Error: `%s`\n\n", sum.Error)
		return
	}
	fmt.Fprintf(sb, "Enriched %d nodes.\n\n", sum.Enriched)
	if len(sum.PerRepo) > 0 {
		sb.WriteString("| Repo prefix | Nodes enriched |\n")
		sb.WriteString("|-------------|----------------|\n")
		for repo, count := range sum.PerRepo {
			fmt.Fprintf(sb, "| `%s` | %d |\n", repo, count)
		}
		sb.WriteString("\n")
	}
}

func formatTimestamp(ts int64) string {
	if ts == 0 {
		return "—"
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}
