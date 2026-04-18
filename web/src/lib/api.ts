import type {
  HealthResponse, ToolInfo, GraphStats, ToolResponse, GraphData,
  SubGraph, GortexNode, GraphChangeEvent, CommunityResult, Community,
  Process, IndexHealth,
} from './types'

// Single base URL for the gortex server (http://.../v1/*). The old
// NEXT_PUBLIC_GORTEX_WEB_URL is kept as a fallback only for backwards
// compatibility with any existing .env files; nothing should set it
// going forward.
const SERVER_URL = process.env.NEXT_PUBLIC_GORTEX_URL
  || process.env.NEXT_PUBLIC_GORTEX_WEB_URL
  || 'http://localhost:4747'

// Optional bearer token. Required when the server was started with
// --auth-token / $GORTEX_SERVER_TOKEN; otherwise leave unset.
const AUTH_TOKEN = process.env.NEXT_PUBLIC_GORTEX_TOKEN || ''

// --- Server API ---

function authHeaders(): HeadersInit {
  return AUTH_TOKEN ? { Authorization: `Bearer ${AUTH_TOKEN}` } : {}
}

async function serverFetch(path: string, options?: RequestInit): Promise<Response> {
  const res = await fetch(`${SERVER_URL}${path}`, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      ...authHeaders(),
      ...options?.headers,
    },
  })
  if (!res.ok) {
    const text = await res.text()
    throw new Error(`Server API error ${res.status}: ${text}`)
  }
  return res
}

async function callTool(name: string, args: Record<string, unknown> = {}): Promise<string> {
  const res = await serverFetch(`/v1/tools/${name}`, {
    method: 'POST',
    body: JSON.stringify({ arguments: args }),
  })
  const data: ToolResponse = await res.json()
  if (data.isError) {
    throw new Error(data.content?.[0]?.text || 'Tool call failed')
  }
  return data.content?.map(c => c.text).join('\n') || ''
}

async function callToolJSON<T>(name: string, args: Record<string, unknown> = {}): Promise<T> {
  const text = await callTool(name, args)
  try {
    return JSON.parse(text) as T
  } catch {
    // Some tools return plain text (e.g. compact format) instead of JSON.
    // Wrap it as an empty result so callers get a valid object.
    return { nodes: [], edges: [], text } as unknown as T
  }
}

// --- Public API ---

export const api = {
  // Health & stats
  health: async (): Promise<HealthResponse> => {
    const res = await serverFetch('/v1/health')
    return res.json()
  },

  tools: async (): Promise<ToolInfo[]> => {
    const res = await serverFetch('/v1/tools')
    return res.json()
  },

  stats: async (): Promise<GraphStats> => {
    const res = await serverFetch('/v1/stats')
    return res.json()
  },

  // Full brief-graph dump for force-directed rendering. Optional
  // project / repo filters scope the dump the same way MCP tools do.
  getGraph: async (opts?: { project?: string; repo?: string }): Promise<GraphData> => {
    const qs = new URLSearchParams()
    if (opts?.project) qs.set('project', opts.project)
    if (opts?.repo) qs.set('repo', opts.repo)
    const suffix = qs.toString() ? `?${qs}` : ''
    const res = await serverFetch(`/v1/graph${suffix}`)
    return res.json()
  },

  getFileGraph: async (path: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_file_summary', { path })
  },

  getCluster: async (id: string, radius = 2): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_cluster', { id, radius })
  },

  // MCP tool wrappers
  searchSymbols: async (query: string, limit = 20): Promise<string> => {
    return callTool('search_symbols', { query, limit, compact: true })
  },

  getSymbol: async (id: string): Promise<GortexNode | null> => {
    try {
      return await callToolJSON<GortexNode>('get_symbol', { id })
    } catch { return null }
  },

  getSymbolSource: async (id: string): Promise<string> => {
    const result = await callTool('get_symbol_source', { id })
    try {
      const parsed = JSON.parse(result)
      return parsed.source || result
    } catch { return result }
  },

  getSymbolSignature: async (id: string): Promise<string> => {
    return callTool('get_symbol_signature', { id })
  },

  getCommunities: async (): Promise<CommunityResult> => {
    return callToolJSON<CommunityResult>('get_communities', {})
  },

  getCommunity: async (id: string): Promise<Community> => {
    return callToolJSON<Community>('get_community', { id })
  },

  getProcesses: async (): Promise<{ processes: Process[] }> => {
    return callToolJSON<{ processes: Process[] }>('get_processes', {})
  },

  getProcess: async (id: string): Promise<Process> => {
    return callToolJSON<Process>('get_process', { id })
  },

  getCallers: async (id: string, depth = 2): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_callers', { id, depth, compact: true })
  },

  getCallChain: async (id: string, depth = 2): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_call_chain', { id, depth, compact: true })
  },

  findUsages: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('find_usages', { id, compact: true })
  },

  getDependencies: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_dependencies', { id })
  },

  getDependents: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_dependents', { id })
  },

  explainChangeImpact: async (symbolIds: string): Promise<unknown> => {
    return callToolJSON('explain_change_impact', { ids: symbolIds })
  },

  findDeadCode: async (): Promise<unknown> => {
    return callToolJSON('find_dead_code', {})
  },

  findHotspots: async (): Promise<unknown> => {
    return callToolJSON('find_hotspots', {})
  },

  findCycles: async (): Promise<unknown> => {
    return callToolJSON('find_cycles', {})
  },

  indexHealth: async (): Promise<IndexHealth> => {
    return callToolJSON<IndexHealth>('index_health', {})
  },

  graphStats: async (): Promise<GraphStats> => {
    return callToolJSON<GraphStats>('graph_stats', {})
  },

  // Raw tool call
  callTool,
  callToolJSON,

  // SSE. The browser EventSource API can't attach custom headers, so
  // the token is passed as a query string when present — the server
  // accepts ?token=<t> as a fallback for streaming endpoints.
  // Localhost dev (the common case) runs the server unauthenticated,
  // so AUTH_TOKEN is empty and nothing is appended.
  subscribeEvents: (callback: (event: GraphChangeEvent) => void): EventSource => {
    const qs = AUTH_TOKEN ? `?token=${encodeURIComponent(AUTH_TOKEN)}` : ''
    const es = new EventSource(`${SERVER_URL}/v1/events${qs}`)
    es.addEventListener('graph_change', (e) => {
      try {
        const data = JSON.parse(e.data) as GraphChangeEvent
        callback(data)
      } catch { /* ignore parse errors */ }
    })
    return es
  },
}
