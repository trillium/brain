# task-fburn26: unify/prefix cutover blockers

```mermaid
flowchart LR
  subgraph Before
    A1[duplicate id, identical issues row] --> A2[loser skipped WITH its labels/comments/deps] --> A3[silent child-row loss]
    B1[post-repoint catch-up] --> B2[re-insert source child rows] --> B3[deleted labels resurrected]
    C1[RecordStorePrefix] --> C2[INSERT claim] --> C3[DDL implicit commit] --> C4[claim durable without its event]
  end
  subgraph After
    A4[duplicate id] --> A5[child-row digests compared] --> A6[loser has rows winner lacks -> conflict bead, all copies kept]
    B4[post-repoint catch-up] --> B5[only rows added since recorded base commit] --> B6[deletes stay deleted]
    C5[RecordStorePrefix] --> C6[DDL first] --> C7[INSERT claim + event in one tx]
  end
```

Fixed: D-F1, D-F1b, C-F1, C-F2, F-F1. Tests: `go test ./internal/brainunify/ ./internal/storage/issueops/` pass.
Deferred (own beads): C-F3, F-F2/F-F3.
