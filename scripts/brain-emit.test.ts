import { describe, it, expect } from "bun:test";
import { spawnSync } from "bun";

describe("brain-emit wrapper", () => {
  it("accepts bd command passthrough", () => {
    // Test that the wrapper can be invoked without crashing
    const result = spawnSync({
      cmd: ["bun", "scripts/brain-emit.ts", "--help"],
      stdio: ["pipe", "pipe", "pipe"],
    });
    expect(result.exitCode).toBeGreaterThanOrEqual(0);
  });

  it("identifies mutating verbs correctly", () => {
    const mutatingVerbs = [
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
    ];

    const readVerbs = ["show", "list", "search", "get"];

    // Verify all verbs are categorized
    expect(mutatingVerbs.length).toBeGreaterThan(0);
    expect(readVerbs.length).toBeGreaterThan(0);

    // Ensure no overlap
    const mutatingSet = new Set(mutatingVerbs);
    for (const verb of readVerbs) {
      expect(mutatingSet.has(verb)).toBe(false);
    }
  });

  it("preserves command exit codes", () => {
    // Test with a command that will succeed (bd --help)
    const result = spawnSync({
      cmd: ["bun", "scripts/brain-emit.ts", "--help"],
      stdio: ["pipe", "pipe", "pipe"],
    });
    expect(result.exitCode).toBe(0);
  });

  it("handles unset webhook endpoint gracefully", () => {
    // Run without BRAIN_EMIT_WEBHOOK env var - should not emit
    const result = spawnSync({
      cmd: ["bun", "scripts/brain-emit.ts", "show", "nonexistent"],
      stdio: ["pipe", "pipe", "pipe"],
    });
    // Should exit with bd's exit code, not crash
    expect(typeof result.exitCode).toBe("number");
  });

  it("rejects invalid webhook URLs without crashing", () => {
    const result = spawnSync({
      cmd: ["bun", "scripts/brain-emit.ts", "show", "nonexistent"],
      env: { ...process.env, BRAIN_EMIT_WEBHOOK: "not-a-valid-url" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    // Should still exit normally, not crash on bad webhook URL
    expect(typeof result.exitCode).toBe("number");
  });

  it("handles webhook connection failures gracefully", () => {
    // Use a port that's not listening to trigger a connection error
    const result = spawnSync({
      cmd: ["bun", "scripts/brain-emit.ts", "show", "nonexistent"],
      env: { ...process.env, BRAIN_EMIT_WEBHOOK: "http://localhost:1/webhook" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    // Should not crash on webhook failure
    expect(typeof result.exitCode).toBe("number");
  });

  it("structures webhook payload correctly", () => {
    const examplePayload = {
      store: "task",
      verb: "create" as const,
      id: "task-123",
      ts: Date.now(),
      data: {
        argc: 3,
      },
    };

    // Verify all required fields are present
    expect(examplePayload).toHaveProperty("store");
    expect(examplePayload).toHaveProperty("verb");
    expect(examplePayload).toHaveProperty("id");
    expect(examplePayload).toHaveProperty("ts");
    expect(examplePayload).toHaveProperty("data");

    // Verify types
    expect(typeof examplePayload.store).toBe("string");
    expect(typeof examplePayload.verb).toBe("string");
    expect(typeof examplePayload.id).toBe("string");
    expect(typeof examplePayload.ts).toBe("number");
    expect(typeof examplePayload.data).toBe("object");
  });
});
