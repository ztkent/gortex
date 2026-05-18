package wiki

// promptVersion is bumped when a prompt template changes so the
// enhance-cache discards stale entries automatically.
const promptVersion = 1

// promptCommunity asks the LLM to write a short narrative paragraph
// about a community given the rendered markdown body + structured
// context (size, cohesion, file count). The LLM never receives source
// code — only signatures already inside the markdown.
const promptCommunity = `You are documenting a software community detected by graph clustering.
You have been given the markdown that the template generator already produced.

Your job: rewrite ONLY the "## Files" / "## Entry Points" / "## Execution Flows" introduction sentences and the page title's lead paragraph so the document reads as well-written engineering documentation. Keep every table, list, and code block intact verbatim. Do not invent symbols or files. Output ONLY the rewritten markdown, no commentary, no fences.

Inputs:
- Page title: %s
- Structured context: %s
- Markdown body:
%s
`

// promptProcess asks the LLM to add a short narrative explanation of
// the process before the steps table.
const promptProcess = `You are documenting an execution flow extracted from call-graph DFS preorder.

Your job: rewrite ONLY the lead paragraph (between the page title and the "## Flow" header) so it explains in 1-2 sentences what this flow accomplishes. Keep the Mermaid block, the steps table, and the file list intact verbatim. Do not invent symbols. Output ONLY the rewritten markdown, no commentary, no fences.

Inputs:
- Page title: %s
- Markdown body:
%s
`

// promptArchitecture asks the LLM to write an overview paragraph that
// names the load-bearing communities and the main dependency
// direction.
const promptArchitecture = `You are documenting a software architecture.

Your job: rewrite ONLY the paragraph before the "## Communities by Size" header (or the corresponding intro sentence in an index page) so it names the most important communities and the dominant dependency direction. Keep every table, Mermaid block, and code block intact verbatim. Do not invent symbols or communities. Output ONLY the rewritten markdown, no commentary, no fences.

Inputs:
- Page title: %s
- Markdown body:
%s
`
