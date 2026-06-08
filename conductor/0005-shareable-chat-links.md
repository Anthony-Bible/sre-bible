# Issue 6: Shareable Chat Links / Scorecard Export

## 1. Product Requirements (Business Scope)

### 📋 Problem Statement
When a recruiter pastes a job description and the agent generates a highly accurate "Fit Scorecard," the recruiter's immediate instinct is to share it with the Hiring Manager. Currently, they have to take screenshots or copy-paste raw text, which ruins the formatting and Citations UX.

### 💡 Business Value / Why
1. **Frictionless Sharing:** Allows recruiters to instantly forward Anthony's Fit Scorecard to decision-makers.
2. **Viral Coefficient:** Every shared link drives a new potential stakeholder directly to `sre.bible`, demonstrating the platform's capability firsthand.
3. **Preserving Context:** Ensures the Hiring Manager sees the exact grounding citations and formatting.

### 📖 User Stories
* **Story 1:** As a technical recruiter, I want to click a "Share" button to generate a read-only link to my current conversation.
* **Story 2:** As a Hiring Manager opening a shared link, I want to read the conversation and click the citations to verify the facts, but I should not be able to type new messages as the original recruiter.

---

## 2. Technical Specification

### Share Generation & UI
- Add a "Share Chat" icon/button at the top right of the chat container.
- When clicked, it copies a URL like `https://sre.bible/share/<session-id>` to the user's clipboard and shows a "Copied!" toast.

### Backend Routing (`internal/server/handlers.go`)
- Create a new `GET /share/{id}` route.
- This route serves a modified HTML template (e.g., `share.html` or `index.html` with a special flag).
- The modified template:
  1. Does **not** render the input `<form>` or text area.
  2. Renders a banner at the top: *"You are viewing a shared conversation with Anthony Bible's Resume Agent. [Start your own chat]"*
  3. Fetches the history using the provided `{id}`.

### Database Access Control
- Currently, `GET /messages` requires the user to own the `X-Session-Id`.
- For shared links, we need to allow read-only access to messages for a given session ID.
- Since session IDs are UUIDv4 (unguessable), the ID itself acts as a capability token for reading. We can add a `GET /api/share/{id}/messages` endpoint that returns the message history without requiring the standard Turnstile/Ownership checks (or simply bypass the ownership check if a specific read-only token is used).

---

## 3. Verification & Test Plan

### Priority Assessment
1. **Priority 1: Read-Only Enforcement.** A user viewing a shared link MUST NOT be able to append new messages to that session. The `POST /chat` endpoint must rigorously validate the active local session vs. the shared URL session.
2. **Priority 2: PII Security.** Ensure no private emails (from the `send_contact_email` drafts) leak into the shared view. (Note: contact emails are drafted via Tool Calls which do not store the email body in the standard message text, but we must verify this).

### Verification Checklist
- [ ] Clicking "Share" copies the correct URL to clipboard.
- [ ] Navigating to `/share/{uuid}` loads the read-only UI (no chat input).
- [ ] The shared history accurately renders Markdown, tables, and Citations.
- [ ] Attempting to `POST /chat` via cURL using a shared UUID without the proper client-side Turnstile/Session ownership is rejected by the server.