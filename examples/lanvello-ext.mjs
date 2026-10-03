// lanvello provider for pi-family harnesses (omp, prime-agent).
// load with: -e /tmp/opencode/evals/lanvello-ext.mjs
// key comes from LANVELLO_API_KEY env.
export default function lanvello(pi) {
  const baseUrl = process.env.LANVELLO_BASE_URL || "http://127.0.0.1:11448/v1";
  const apiKey = process.env.LANVELLO_API_KEY || "noop";
  pi.registerProvider("lanvello", {
    baseUrl,
    api: "openai-completions",
    apiKey,
    models: [
      {
        id: "fledge-alpha-free",
        name: "Fledge Alpha Free (lanvello/tor)",
        reasoning: false,
        input: ["text"],
        contextWindow: 128000,
        maxTokens: 8192,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      },
      {
        id: "muse-spark-1.3-contributor-free",
        name: "Muse Spark 1.3 Contributor Free (lanvello/tor)",
        reasoning: true,
        input: ["text"],
        contextWindow: 200000,
        maxTokens: 8192,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      },
    ],
  });
}
