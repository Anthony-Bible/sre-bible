# Feature Roadmap

This document serves as the index for the upcoming high-impact features planned for `sre.bible`. Each feature has been thoroughly scoped by our specialized subagents (Technical Requirements Gatherer, PEM/PM Scoper, and TDD Review Agent) to ensure business value, technical viability, and robust testing standards.

## Planned Features

1. **[Secret "Deadpool Mode" Easter Egg](./0001-deadpool-mode.md)**
   - **Goal:** Break recruiter monotony and showcase elite prompt engineering with a PG-13, fourth-wall-breaking persona.
   - **Implementation:** Context-scoped persona swapping via Go `context.Context` and persistent `deadpool_mode` toggles in the Postgres `sessions` table.

2. **[Dark Mode Toggle (Retina-Saver)](./0002-dark-mode.md)**
   - **Goal:** Provide a developer-empathetic, eye-strain-reducing UX that SREs and night-owl recruiters expect.
   - **Implementation:** CSS variable theming, OS-level `prefers-color-scheme` detection, and `localStorage` persistence with FOUC prevention.

3. **[ATS-Compliant Resume Downloader](./0003-resume-downloader.md)**
   - **Goal:** Allow corporate recruiters to easily pull a parseable PDF for their Applicant Tracking Systems.
   - **Implementation:** Add a direct link to `https://anthony.bible/downloads/resume.pdf` in the header and update the LLM prompt to offer it naturally.

4. **[Conversation Feedback (LLM-as-a-Judge)](./0004-conversation-feedback.md)**
   - **Goal:** Gather empirical, user-driven data on LLM answer quality to detect hallucination edge cases and prompt drifts.
   - **Implementation:** Add a `feedback` column to the `messages` table, expose a `POST /chat/message/{id}/feedback` endpoint, and integrate thumbs-up/down icons asynchronously.

5. **[Shareable Chat Links / Scorecard Export](./0005-shareable-chat-links.md)**
   - **Goal:** Allow frictionless sharing of Anthony's Fit Scorecard with Hiring Managers, preserving the context and citations UX.
   - **Implementation:** Implement a `GET /share/{id}` route for read-only viewing of a session's history without chat input capabilities.

---
*All scopes have been drafted and are ready for implementation via Green Phase Agents.*