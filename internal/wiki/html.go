package wiki

import (
	"fmt"
	"strings"
)

// htmlIndexTemplate is a self-contained viewer that loads the per-repo
// markdown pages (relative to the wrapping HTML file) and renders them
// via marked + Mermaid.js from CDN. It's a minimal nav-by-anchor
// experience: clicking the sidebar links replaces the right pane's
// content with the matching markdown file fetched and rendered
// client-side.
const htmlIndexTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>__TITLE__</title>
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <style>
    body { font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; margin: 0; color: #222; }
    .wrap { display: flex; min-height: 100vh; }
    nav { width: 280px; border-right: 1px solid #e5e7eb; padding: 18px; background: #f9fafb; overflow-y: auto; }
    nav h1 { font-size: 16px; margin: 0 0 12px 0; }
    nav a { display: block; color: #1d4ed8; text-decoration: none; padding: 4px 0; font-size: 13px; }
    nav a:hover { text-decoration: underline; }
    main { flex: 1; padding: 32px 48px; max-width: 980px; }
    pre { background: #f3f4f6; padding: 12px; border-radius: 6px; overflow-x: auto; }
    code { background: #f3f4f6; padding: 1px 4px; border-radius: 3px; }
    table { border-collapse: collapse; margin: 12px 0; }
    th, td { border: 1px solid #e5e7eb; padding: 6px 10px; text-align: left; }
    th { background: #f9fafb; }
    .mermaid { background: #fff; padding: 8px; }
    section { display: none; }
    section.active { display: block; }
  </style>
</head>
<body>
<div class="wrap">
  <nav>
    <h1>__TITLE__</h1>
    <a href="#" data-page="index.md">Index</a>
    <a href="#" data-page="architecture.md">Architecture</a>
    <a href="#" data-page="changelog.md">Changelog</a>
    <a href="#" data-page="analysis/hotspots.md">Hotspots</a>
    <a href="#" data-page="analysis/cycles.md">Cycles</a>
    <a href="#" data-page="analysis/semantic.md">Semantic</a>
    <a href="#" data-page="contracts/api-surface.md">API contracts</a>
    <a href="#" data-page="communities/" id="nav-communities">Communities &raquo;</a>
  </nav>
  <main>
    <section id="content" class="active"><p>Loading...</p></section>
  </main>
</div>

<script src="https://cdn.jsdelivr.net/npm/marked/marked.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.min.js"></script>
<script>
  mermaid.initialize({ startOnLoad: false });
  const content = document.getElementById('content');
  async function load(page) {
    if (!page || page.endsWith('/')) return;
    try {
      const resp = await fetch(page, {cache: 'no-store'});
      const md = await resp.text();
      // Strip frontmatter (--- ... ---) before rendering.
      const body = md.replace(/^---\n[\s\S]*?\n---\n+/, '');
      content.innerHTML = marked.parse(body);
      // Rewrite mermaid blocks (marked emits as <pre><code class="language-mermaid">).
      document.querySelectorAll('pre code.language-mermaid').forEach((el, i) => {
        const wrapper = document.createElement('div');
        wrapper.className = 'mermaid';
        wrapper.textContent = el.textContent;
        el.parentElement.replaceWith(wrapper);
      });
      mermaid.run();
    } catch (e) {
      content.innerHTML = '<p>Failed to load <code>' + page + '</code>: ' + e.message + '</p>';
    }
  }
  document.querySelectorAll('nav a').forEach(a => {
    a.addEventListener('click', e => {
      e.preventDefault();
      load(a.dataset.page);
    });
  });
  load('index.md');
</script>
</body>
</html>
`

// RenderHTMLIndex builds a self-contained HTML viewer for one repo.
// It does not enumerate every community page at build time — the user
// navigates by clicking the markdown links inside the rendered pages.
// The Communities entry is a placeholder; community pages are reached
// from within index.md.
func RenderHTMLIndex(repoSlug string, opts Options) string {
	title := repoSlug + " — Gortex wiki"
	if opts.Project != "" {
		title = opts.Project + " · " + title
	}
	out := strings.ReplaceAll(htmlIndexTemplate, "__TITLE__", htmlEscape(title))
	_ = fmt.Sprintf // keep fmt usable for future tweaks
	return out
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	)
	return r.Replace(s)
}
