# Issue 3: ATS-Compliant Resume Downloader

## 1. Product Requirements (Business Scope)

### 📋 Problem Statement
While the conversational AI agent is an incredible showcase of skills, enterprise Applicant Tracking Systems (ATS) like Workday or Greenhouse require a standard, parseable PDF resume. Recruiters using the site will ultimately need to download the official PDF to move Anthony into their official hiring pipeline.

### 💡 Business Value / Why
1. **Reduce Friction:** Do not make recruiters hunt for a download link or ask the agent for it.
2. **ATS Compatibility:** Ensure the resume format is what parsing systems expect, preventing parsing errors.
3. **Clear CTA:** A prominent download button acts as a strong Call to Action.

### 📖 User Stories
* **Story 1:** As a corporate recruiter, I enjoyed chatting with the agent, but now I need to upload Anthony's resume to our ATS. I want a one-click download button.
* **Story 2:** As a viewer, I want the agent itself to offer the download link if I explicitly ask "Can I download your resume?".

---

## 2. Technical Specification

### Static Asset Routing
- The official resume is hosted at `https://anthony.bible/downloads/resume.pdf`.
- We will add a prominent "Download PDF Resume" button in the header of `internal/server/templates/index.html` that links directly to this URL with `target="_blank"`.

### Suggested Questions Integration
- Update `defaultSuggestedQuestions()` in `internal/server/server.go` to include: `"Can I download your official PDF resume?"`
- Update the system prompt (`internal/rag/prompt.go`) to instruct the LLM:
  * "If a user asks to download Anthony's resume, direct them to click the download button in the header, or provide them with this link: https://anthony.bible/downloads/resume.pdf"

### UI Additions
- Place the Download button in the `<header>` element, aligned to the right. Use a distinct button style (e.g., a hollow button with a download icon) to distinguish it from the site title.

---

## 3. Verification & Test Plan

### Priority Assessment
1. **Priority 1: Link Validity.** The URL must return a 200 OK and serve the PDF.
2. **Priority 2: Prompt Adherence.** The agent must reliably output the link when explicitly asked.

### Verification Checklist
- [ ] Header button renders correctly on both desktop and mobile viewports.
- [ ] Clicking the button opens the PDF in a new tab.
- [ ] Asking the agent "download resume" yields the correct `https://anthony.bible/downloads/resume.pdf` link.