# Issue 1: Secret "Deadpool Mode" Easter Egg

## 1. Product Requirements (Business Scope)

### 📋 Problem Statement
Recruiters and hiring managers sift through hundreds of identical, buzzword-filled LinkedIn profiles and dry CVs every single week. We need to inject a high-voltage jolt of pure dopamine directly into the recruiting pipeline while showcasing elite prompt engineering skills.

### 💡 Business Value / Why
1. **Break the Recruiter Monotony:** Be the one portfolio they remember at the end of a grueling sourcing sprint.
2. **Showcase Elite Prompt Engineering:** Crafting a highly constrained, complex, in-character persona that dynamically references grounding data without hallucinating is a masterclass in LLM orchestration.
3. **Organic Viral Loop:** Create a feature hilarious and delightful enough that recruiters post it on LinkedIn.

### 👥 Stakeholders & Personas
* **Primary Persona:** Sourcing Sally (The Bored Recruiter)
* **Secondary Persona:** HM Harry (The Technical Hiring Manager)
* **Corporate HR:** The humor *must* remain "Corporate Safe" (PG-13, no real vulgarity).

---

## 2. Technical Specification

### Toggle & Propagation Mechanism
- **Trigger**: Visitor adds `?mode=deadpool` to the URL, OR types `/deadpool` or `chimichanga` in the chat input.
- **Client-Side Interception**: The frontend JS intercepts the text commands (`/deadpool`, `chimichanga`) *before* sending a network request. It updates local storage, renders a local message indicating activation, and drops the input.
- **Client-Side Storage**: Stored as `true` in `sessionStorage`.
- **API Headers**: Client includes `X-Deadpool-Mode: true` in `GET /messages` and `POST /chat`.
- **Database**: Add `deadpool_mode BOOLEAN` to `sessions` table. Server intercepts header and updates DB.

### Context-Scoped Persona Swapping
- Define `deadpoolModeKey` in package `rag`.
- HTTP handler wraps the request context via `context.WithValue`.
- Split the system prompt into two parts: `rag.BaseSystemPrompt` (hard rules, tools, PII constraints) and `rag.StandardPersona` / `rag.DeadpoolPersona` (voice modifiers).
- Modify `llm.NewClient` to accept `basePrompt` and `personas map[rag.PersonaMode]string`.
- Inside `llm.Client.StreamAnswer`, check context and append the active persona to the `System` text block array: `System: []anthropic.TextBlockParam{{Text: c.basePrompt}, {Text: activePersona}}`.

---

## 3. Verification & Test Plan

### Priority Assessment
1. **Priority 1: Persona Isolation.** The standard professional mode *must* remain the default.
2. **Priority 2: PII Rule Enforcement.** The Deadpool prompt must strictly enforce using the `send_contact_email` tool and LinkedIn/GitHub channels.
3. **Priority 3: Clean Context Propagation.** Must use idiomatic Go `context.Context`.

### Verification Checklist
- [ ] No reflection or generic type-safety bypasses.
- [ ] Existing sessions default to `standard` without breaking.
- [ ] Deadpool prompt strictly prohibits giving out phone numbers or direct emails.
- [ ] Tools match standard prompt exactly (`send_contact_email`, `match_job_description`, etc.).