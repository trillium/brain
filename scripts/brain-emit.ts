#!/usr/bin/env bun
import { spawnSync } from "bun";
import * as path from "path";

type MutatingVerb =
  | "create"
  | "patch"
  | "label"
  | "close"
  | "open"
  | "tag"
  | "comment"
  | "update"
  | "set-state"
  | "delete";

interface EmitPayload {
  store: string;
  verb: MutatingVerb;
  id: string;
  ts: number;
  data: Record<string, unknown>;
}

function detectStore(): string {
  // Try to get store name from script name (e.g., 'brain-emit' -> 'brain')
  // Fall back to environment or 'unknown'
  const scriptName = path.basename(Bun.main);
  const match = scriptName.match(/^([a-z-]+)-emit/);
  if (match) {
    return match[1];
  }
  return process.env.BEADS_STORE || "unknown";
}

function parseArgs(): { verb?: string; id?: string; remaining: string[] } {
  const args = Bun.argv.slice(2);
  if (args.length === 0) {
    return { remaining: args };
  }

  let verb: string | undefined;
  let id: string | undefined;

  for (let i = 0; i < args.length; i++) {
    if (isKnownVerb(args[i])) {
      verb = args[i];
      // ID might follow the verb
      if (i + 1 < args.length && !args[i + 1].startsWith("-")) {
        id = args[i + 1];
      }
      break;
    }
  }

  return { verb, id, remaining: args };
}

function isKnownVerb(s: string): boolean {
  const verbs = new Set([
    "create",
    "patch",
    "label",
    "close",
    "open",
    "tag",
    "comment",
    "update",
    "show",
    "list",
    "search",
    "get",
    "set-state",
    "delete",
  ]);
  return verbs.has(s);
}

function isMutatingVerb(verb: string): boolean {
  const mutating = new Set<string>([
    "create",
    "patch",
    "label",
    "close",
    "open",
    "tag",
    "comment",
    "update",
    "set-state",
    "delete",
  ]);
  return mutating.has(verb);
}

async function emitWebhook(
  payload: EmitPayload,
  endpoint: string
): Promise<void> {
  try {
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 5000);

    await fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
      signal: controller.signal,
    }).catch(() => null);

    clearTimeout(timeoutId);
  } catch {
    // Silently swallow all errors
  }
}

async function main() {
  const store = detectStore();
  const { verb, id, remaining } = parseArgs();
  const endpoint = process.env.BRAIN_EMIT_WEBHOOK;

  // Run the actual bd command
  const result = spawnSync({
    cmd: ["bd", ...remaining],
    stdio: ["inherit", "inherit", "inherit"],
  });

  // Emit webhook if all conditions are met
  if (result.success && endpoint && verb && isMutatingVerb(verb)) {
    const payload: EmitPayload = {
      store,
      verb: verb as MutatingVerb,
      id: id || "unknown",
      ts: Date.now(),
      data: {
        argc: remaining.length,
      },
    };

    emitWebhook(payload, endpoint).catch(() => {
      // Silently ignore webhook errors
    });
  }

  process.exit(result.exitCode);
}

main().catch(() => {
  process.exit(1);
});
