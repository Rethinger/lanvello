// lanvello-bridge — hand an api caller's request to this client.
//
// The free tier leases a session to the running client and to nobody else, so
// lanvello's api asks this client to make the call. The caller's own system
// prompt, history and tool schemas arrive in a spool file the gateway writes
// next to the session it opened; this plugin rebuilds the model request from
// it, and refuses to run the caller's tools — their results come back from the
// api caller, not from inside the client.
//
// Install by copying this file into ~/.config/opencode/plugins/ (or
// .opencode/plugins/ inside a project). The spool directory is
// $LANVELLO_BRIDGE_DIR, defaulting to <state>/lanvello/bridge, which is where
// `lanvello serve` parks requests by default. Without this plugin the bridge
// still answers, but with the client's own agent prompt and its own tools.

const fs = require("fs");
const path = require("path");

const DIR =
  process.env.LANVELLO_BRIDGE_DIR ||
  path.join(
    process.env.XDG_STATE_HOME || path.join(process.env.HOME || ".", ".local", "state"),
    "lanvello",
    "bridge",
  );

type ParkedMessage = {
  role: string;
  content?: string;
  tool_calls?: Array<{ id?: string; name: string; arguments?: unknown }>;
  tool_call_id?: string;
};

type ParkedTool = { name: string; description?: string; parameters?: unknown };

type Parked = {
  sessionId?: string;
  system?: string;
  messages?: ParkedMessage[];
  tools?: ParkedTool[];
  model?: string;
};

type ContextHook = {
  sessionID?: string;
  system?: unknown[];
  messages?: unknown[];
  tools?: Record<string, unknown>;
};

type ExecPayload = { id?: string; sessionID: string; tool: string; input?: unknown };

type PluginCtx = {
  session?: { hook?: (name: string, fn: (input: ContextHook) => void) => unknown };
  tool?: { hook?: (name: string, fn: (payload: unknown) => void) => unknown };
};

function parked(sessionID: string | undefined): Parked | null {
  if (!sessionID) return null;
  try {
    const raw: unknown = JSON.parse(fs.readFileSync(path.join(DIR, sessionID + ".json"), "utf8"));
    if (!raw || typeof raw !== "object") return null;
    return raw as Parked;
  } catch (e) {
    return null;
  }
}

// asExec narrows the untrusted hook payload down to the call it describes.
function asExec(payload: unknown): ExecPayload | null {
  if (!payload || typeof payload !== "object") return null;
  const p = payload as Record<string, unknown>;
  if (typeof p.sessionID !== "string" || typeof p.tool !== "string") return null;
  return {
    id: typeof p.id === "string" ? p.id : undefined,
    sessionID: p.sessionID,
    tool: p.tool,
    input: p.input,
  };
}

// buildMessage turns one caller turn into the client's message shape: text for
// text, and the tool traffic spelled out, so the model still sees what it asked
// for and what came back.
function buildMessage(m: ParkedMessage): { role: string; content: unknown[]; metadata: object } | null {
  const parts: Array<{ type: string; text: string }> = [];
  if (m.content && m.role === "tool") {
    parts.push({ type: "text", text: `[tool result${m.tool_call_id ? " for " + m.tool_call_id : ""}] ${m.content}` });
  } else if (m.content) {
    parts.push({ type: "text", text: m.content });
  }
  for (const c of m.tool_calls || []) {
    parts.push({ type: "text", text: `[tool call ${c.name}(${JSON.stringify(c.arguments ?? {})})]` });
  }
  if (parts.length === 0) return null;
  return { role: m.role === "tool" ? "user" : m.role, content: parts, metadata: {} };
}

export default {
  id: "lanvello-bridge",
  setup: async (ctx: PluginCtx) => {
    if (ctx.session?.hook) {
      await ctx.session.hook("context", (input: ContextHook) => {
        const req = parked(input.sessionID);
        if (!req) return;

        // the caller's system prompt replaces the client's own agent prompt
        input.system = req.system ? [{ type: "text", text: req.system }] : [];

        if (Array.isArray(req.messages)) {
          const built = req.messages.map(buildMessage).filter((m) => m !== null);
          if (built.length > 0) input.messages = built;
        }

        // and its tools replace the client's — including replacing them with
        // nothing, so a tool-less api request cannot run bash in this directory
        const tools: Record<string, unknown> = {};
        for (const t of req.tools || []) {
          if (!t || !t.name) continue;
          tools[t.name] = {
            description: t.description || "",
            input: t.parameters || { type: "object", properties: {} },
          };
        }
        input.tools = tools;
      });
    }

    if (ctx.tool?.hook) {
      await ctx.tool.hook("execute.before", (payload: unknown) => {
        const call = asExec(payload);
        if (!call) return;
        const req = parked(call.sessionID);
        if (!req) return;
        if (!(req.tools || []).some((t) => t && t.name === call.tool)) return;
        // this call is the api caller's to run: record it for the gateway and
        // stop the client from executing it here
        try {
          fs.appendFileSync(
            path.join(DIR, call.sessionID + ".calls.jsonl"),
            JSON.stringify({ id: call.id, name: call.tool, arguments: call.input ?? {} }) + "\n",
          );
        } catch (e) {}
        throw new Error(
          `lanvello-bridge: ${call.tool} belongs to the api caller; its result arrives in the next request`,
        );
      });
    }
  },
};
