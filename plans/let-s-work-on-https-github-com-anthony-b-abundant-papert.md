# Refactor: Move `ContactEmail` to `internal/email` (restore hexagonal import direction)

## Context

The contact-email feature (PR #9) landed with an inverted dependency: `ContactEmail` is defined in `internal/rag/domain.go`, forcing both `internal/email` (the contact-email domain service) and `internal/db` (the persistence adapter) to import `rag` just to use a struct that semantically belongs to the email domain.

**Current import graph (wrong):**
```
email → rag        (domain service depends on a sibling domain)
db    → email + rag (adapter bridges two domains)
```

**Target import graph:**
```
rag → email        (rag's EmailSender port references email's value type)
email → (nothing internal)
db → email         (adapter implements email.ContactRepository only)
llm → rag + email  (constructs email.ContactEmail for the tool call)
```

`internal/email` becomes a self-contained hexagon: it owns its value type (`ContactEmail`) and its driven ports (`ContactRepository`, `Transport`). `rag` keeps its own driven port (`EmailSender` — defined where consumed) but references `email.ContactEmail` as the payload type.

**Cycle constraint:** once `rag` imports `email`, `email` can no longer import `rag`. So `Service.Bind` cannot return `rag.EmailSender`. Fix: `Bind` returns an exported concrete type (`*BoundSender`), and `cmd/server/main.go` adapts it to `rag.EmailerFactory` with a one-line closure (structural typing does the rest).

## Changes

### 1. `internal/email/sender.go`
- Define `ContactEmail` here (moved verbatim from `rag/domain.go`, comment kept: "a Viewer's outbound message to the Owner").
- Drop the `rag` import entirely.
- `ContactRepository.RecordSend` signature: `e rag.ContactEmail` → `e ContactEmail`.
- Rename `boundEmailer` → exported `BoundSender` (it now crosses the package boundary).
- `Bind(sessionID string) rag.EmailSender` → `Bind(sessionID string) *BoundSender`. Update doc comment: returns a session-bound sender that structurally satisfies `rag.EmailSender`.
- `sendContactEmail` / `SendContactEmail` params: `rag.ContactEmail` → `ContactEmail`.

### 2. `internal/rag/domain.go`
- Remove the `ContactEmail` struct.
- Import `internal/email`; `EmailSender.SendContactEmail` takes `email.ContactEmail`.
- `EmailerFactory` unchanged (`func(sessionID string) EmailSender`).

### 3. `internal/db/contact.go`
- Drop the `rag` import.
- `RecordSend` takes `email.ContactEmail`.
- Compile-time assert `var _ email.ContactRepository = (*ContactStore)(nil)` already exists — unchanged.

### 4. `internal/llm/llm.go`
- `runTool` case `toolSendContactEmail` (line ~263): construct `email.ContactEmail{...}` instead of `rag.ContactEmail{...}`; add the `internal/email` import.

### 5. `cmd/server/main.go`
- `setupEmailer` (line ~192): `*out = emailSvc.Bind` → `*out = func(sid string) rag.EmailSender { return emailSvc.Bind(sid) }`.
- Compile-time assertion block at top (line ~31) already asserts `email.ContactRepository = (*db.ContactStore)(nil)` — unchanged. Add `var _ rag.EmailSender = (*email.BoundSender)(nil)` to catch structural drift.

### 6. Tests (mechanical type swaps)
- `internal/email/sender_test.go` — `rag.ContactEmail` → `ContactEmail`; drop `rag` import.
- `internal/db/contact_test.go` — `rag.ContactEmail` → `email.ContactEmail`.
- `internal/llm/llm_test.go` — stub emailer signature → `email.ContactEmail`.
- `internal/rag/pipeline_test.go` — stub emailer signature → `email.ContactEmail`.

## Verification

1. `go build ./... && go vet ./... && go test ./...` — all green, no behavior change expected.
2. `golangci-lint run` (if configured) — no new findings.
3. Confirm import direction: `grep -rn '"github.com/Anthony-Bible/sre-bible/internal/rag"' internal/email internal/db` returns nothing.
4. Commit and push to `worktree-add-email-tool` (updates PR #9).
