# Issue 5: Conversation Feedback (LLM-as-a-Judge Metrics)

## 1. Product Requirements (Business Scope)

### 📋 Problem Statement
As the system scales, it is difficult to manually read through every session log to determine if the agent is hallucinating, providing weak answers, or failing to parse complex Job Descriptions. We need empirical, user-driven data on answer quality.

### 💡 Business Value / Why
1. **Continuous Improvement:** Identifies which sources or chunks need re-ingestion or better descriptions.
2. **Detecting Prompt Drifts:** Provides an early warning system if the system prompt degrades over time.
3. **User Engagement:** Gives the user a sense of agency and interactivity.

### 📖 User Stories
* **Story 1:** As a user, if the agent gives me an incredibly accurate answer, I want to give it a thumbs-up to show my appreciation.
* **Story 2:** As the Owner (Anthony), I want to query the database and sort by "thumbs down" messages so I can investigate hallucination edge-cases.

---

## 2. Technical Specification

### Database Schema Migration
`internal/db/migrations/0010_add_message_feedback.sql`
```sql
ALTER TABLE messages ADD COLUMN feedback SMALLINT DEFAULT 0;
-- Convention: -1 = Thumbs Down, 0 = Neutral/None, 1 = Thumbs Up
```

### API Endpoint
- Create a new route `POST /chat/message/{id}/feedback`.
- Accepts JSON body: `{"feedback": 1}` or `{"feedback": -1}`.
- Validates the `X-Session-Id` header to ensure the user can only rate messages within their own session.
- Updates the `messages` table row matching `{id}`.

### UI Integration (`index.html`)
- Modify the message DOM construction in JavaScript to append a small, elegant thumbs-up/down icon group at the bottom right of **assistant bubbles only**.
- When clicked, immediately update the UI state (e.g., highlight the clicked thumb) and dispatch a background `fetch` to the feedback endpoint.
- Persist the chosen state visually so it survives `GET /messages` polling (which means `GET /messages` must return the `feedback` field).

---

## 3. Verification & Test Plan

### Priority Assessment
1. **Priority 1: Session Security.** Users must not be able to rate messages belonging to other session UUIDs (IDOR vulnerability).
2. **Priority 2: Non-blocking UI.** Feedback submission must be asynchronous and not interrupt the SSE stream or chat flow.

### Verification Checklist
- [ ] Schema migration succeeds and rolls back cleanly.
- [ ] `GET /messages` returns the feedback state for historical messages.
- [ ] `POST /chat/message/{id}/feedback` properly updates the DB.
- [ ] `POST /chat/message/{id}/feedback` returns a 403/404 if the session ID doesn't own the message.
- [ ] UI thumbs appear only on assistant messages, not user messages.