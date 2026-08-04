# Brain Emit: Store Event Webhooks

The `brain-emit` wrapper enables event emission from beads federated stores via webhooks. When configured, it fires HTTP POST notifications for mutating operations (create, patch, close, etc.).

## Installation

```bash
make install-emit-wrappers
```

This installs `~/.local/bin/brain-emit`, a TypeScript/Bun wrapper that intercepts `bd` commands and fires webhooks on success.

## Configuration

Set the `BRAIN_EMIT_WEBHOOK` environment variable to point to your webhook endpoint:

```bash
export BRAIN_EMIT_WEBHOOK=https://your-webhook-endpoint.com/events
```

When unset, the wrapper operates silently (no-op, zero overhead).

## Usage

The wrapper is transparent — it works exactly like `bd`:

```bash
brain-emit task create "My Task"
brain-emit brain label task-123 human
brain-emit task close task-456
```

If `BRAIN_EMIT_WEBHOOK` is set and the command succeeds with a mutating verb, a webhook is fired.

## Webhook Payload

On mutating operations (create, patch, label, close, open, tag, comment, update, set-state, delete), the wrapper POSTs a JSON payload:

```json
{
  "store": "task",
  "verb": "create",
  "id": "task-123",
  "ts": 1723028100000,
  "data": {
    "argc": 2
  }
}
```

### Payload Fields

- **store**: The beads store name (e.g., "task", "brain", "decision")
- **verb**: The mutating operation verb
- **id**: The bead ID (extracted from args or "unknown")
- **ts**: Unix timestamp (milliseconds) when the event was emitted
- **data**: Additional context (argument count, extracted fields, etc.)

## Guarantees

- **Best-effort delivery**: Webhook POST is non-blocking; failures are silently swallowed
- **No command interference**: Webhook errors do NOT affect the underlying `bd` command
- **Read-only bypass**: Read verbs (show, list, search, get) never emit webhooks
- **Transparent passthrough**: Exit codes, stdout, stderr are preserved exactly
- **Zero overhead when unconfigured**: If `BRAIN_EMIT_WEBHOOK` is unset, no webhook logic runs

## Testing

Run the test suite:

```bash
bun test scripts/brain-emit.test.ts
```

Tests verify:
- Command passthrough (exit codes, output)
- Webhook emission on mutating verbs
- No emission on read verbs or unset endpoint
- Webhook failure resilience
- Payload structure correctness
