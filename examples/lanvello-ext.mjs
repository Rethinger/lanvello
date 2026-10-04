// lanvello provider for pi-family harnesses (omp, prime-agent).
// load with: -e examples/lanvello-ext.mjs
//
// The model list is fetched from the gateway at load: the gateway
// pulls it live from opencode through a tor lane, so new free models
// appear here without touching this file. If the gateway is down the
// embedded list below still registers.
//
// key comes from LANVELLO_API_KEY env; base url from LANVELLO_BASE_URL.
const BASE = process.env.LANVELLO_BASE_URL || "http://127.0.0.1:11448/v1";
const KEY = process.env.LANVELLO_API_KEY || "noop";

// offline fallback, same shape the gateway serves
const FALLBACK = [
  { id: "opencode/muse-spark-1.3-contributor-free", name: "Muse Spark 1.3 Contributor Free", reasoning: true, efforts: ["minimal", "low", "medium", "high", "xhigh"], contextWindow: 1048576, maxTokens: 131072, input: ["text", "image", "video", "pdf", "audio"] },
  { id: "opencode/muse-spark-1.2-contributor-free", name: "Muse Spark 1.2 Contributor Free", reasoning: true, efforts: ["minimal", "low", "medium", "high", "xhigh"], contextWindow: 1048576, maxTokens: 131072, input: ["text", "image", "video", "pdf", "audio"] },
  { id: "opencode/space-bunny-free", name: "Space Bunny Free", reasoning: true, efforts: ["low", "medium", "high", "xhigh", "max"], contextWindow: 1048576, maxTokens: 524288, input: ["text", "image", "video"] },
  { id: "opencode/deepseek-v4-flash-free", name: "DeepSeek V4 Flash Free", reasoning: true, efforts: ["low", "high", "max"], contextWindow: 200000, maxTokens: 128000, input: ["text"] },
  { id: "opencode/fledge-alpha-free", name: "Fledge Alpha Free", reasoning: true, efforts: ["low", "high", "max"], contextWindow: 1048576, maxTokens: 131072, input: ["text", "image"] },
  { id: "opencode/longcat-2.5-preview-free", name: "LongCat 2.5 Preview Free", reasoning: true, contextWindow: 1000000, maxTokens: 131072, input: ["text", "image"] },
  { id: "opencode/nemotron-3-ultra-free", name: "Nemotron 3 Ultra Free", reasoning: true, contextWindow: 1000000, maxTokens: 128000, input: ["text"] },
  { id: "opencode/nemotron-3.5-lightning-free", name: "Nemotron 3.5 Lightning Free", reasoning: true, contextWindow: 262144, maxTokens: 262144, input: ["text"] },
  { id: "opencode/ling-3.0-flash-fin-free", name: "Ling 3.0 Flash Fin Free", reasoning: true, contextWindow: 262144, maxTokens: 32768, input: ["text"] },
  { id: "opencode/ling-3.1-flash-free", name: "Ling 3.1 Flash Free", reasoning: true, contextWindow: 262144, maxTokens: 32768, input: ["text"] },
  { id: "opencode/big-pickle", name: "Big Pickle", reasoning: true, contextWindow: 200000, maxTokens: 32000, input: ["text"] },
  { id: "opencode/mimo-v2.6-flash-free", name: "Mimo V2.6 Flash Free", reasoning: true, contextWindow: 200000, maxTokens: 32000, input: ["text", "image", "audio", "video"] },
  { id: "opencode/mimo-v2.5-free", name: "Mimo V2.5 Free", reasoning: true, contextWindow: 200000, maxTokens: 32000, input: ["text", "image", "audio", "video"] },
];

// pi thinking levels; unsupported ones map to null and are hidden
const PI_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

export default async function lanvello(pi) {
  let models = FALLBACK.map(toPiModel);
  try {
    const res = await fetch(BASE.replace(/\/+$/, "") + "/models", {
      headers: KEY ? { Authorization: "Bearer " + KEY } : {},
    });
    if (res.ok) {
      const j = await res.json();
      const list = (j.data || []).map(toPiModel).filter(Boolean);
      if (list.length > 0) models = list;
    }
  } catch {
    // gateway unreachable: keep the offline list
  }
  pi.registerProvider("lanvello", {
    name: "lanvello (opencode free over tor)",
    baseUrl: BASE,
    api: "openai-completions",
    apiKey: KEY,
    models,
  });
}

function toPiModel(m) {
  if (!m || !m.id) return null;
  const efforts = (m.reasoning_options || []).find((o) => o.type === "effort")?.values || [];
  const map = {};
  for (const l of PI_LEVELS) map[l] = efforts.includes(l) ? l : null;
  return {
    id: m.id,
    name: m.name || m.id,
    reasoning: !!m.reasoning,
    // null hides levels the free tier does not serve
    thinkingLevelMap: efforts.length ? map : undefined,
    // without this pi treats an unknown model id as effort-incapable
    // and silently drops reasoning_effort from the request
    compat: { supportsReasoningEffort: true },
    input: m.input && m.input.length ? m.input : ["text"],
    contextWindow: m.context_length || (m.limit && m.limit.context) || 128000,
    maxTokens: m.max_output_tokens || (m.limit && m.limit.output) || 16384,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  };
}
