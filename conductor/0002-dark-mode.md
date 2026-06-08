# Issue 2: Dark Mode Toggle (Retina-Saver)

## 1. Product Requirements (Business Scope)

### 📋 Problem Statement
The primary audience for `sre.bible` consists of Site Reliability Engineers, platform developers, and technical recruiters. These personas overwhelmingly prefer dark-themed IDEs, terminals, and web interfaces. A pure white background causes eye strain, particularly during late-night debugging or sourcing sessions.

### 💡 Business Value / Why
1. **Developer Empathy:** Shows that Anthony understands and caters to developer UX preferences.
2. **Increased Time-on-Site:** Reducing eye strain allows users to comfortably read longer technical descriptions and interact with the agent longer.
3. **Modern Polish:** Dark mode is a table-stakes feature for modern developer portfolios and SaaS platforms.

### 📖 User Stories
* **Story 1:** As a night-owl SRE reviewing Anthony's background at 2 AM, I want the site to automatically detect my OS dark mode preference so I don't get blinded.
* **Story 2:** As a technical recruiter who prefers manual control, I want a visible toggle switch in the UI to switch between light and dark modes explicitly.

---

## 2. Technical Specification

### UI & CSS Variable Implementation
- Convert all hardcoded colors in `internal/server/templates/index.html` to CSS variables (e.g., `--bg-color`, `--text-color`, `--assistant-bubble-bg`).
- Define the default light theme on `:root` and the dark theme on `[data-theme="dark"]`.

### Preference Detection & Persistence
- **OS Detection**: Use the `prefers-color-scheme: dark` media query to set the initial theme state if no manual preference is saved.
- **Manual Toggle**: Add a sun/moon icon toggle in the `<header>`.
- **Client Storage**: Save the user's manual preference in `localStorage` (`localStorage.setItem('theme', 'dark')`).
- **FOUC Prevention**: Add a blocking `<script>` tag immediately inside the `<head>` to read `localStorage` and apply the `data-theme="dark"` attribute before the DOM renders to prevent a Flash of Unstyled Content (FOUC).

### Component Contrast Tuning
- Ensure the blue user bubbles remain legible against a dark background.
- Adjust the Markdown `<code>` and `<pre>` block backgrounds (e.g., `#1e293b` for light, `#0f172a` for dark) so they don't blend into the assistant bubble.
- Ensure the Fit Scorecard `<table>` borders (`#e2e8f0` -> `#334155`) are clearly defined in dark mode.

---

## 3. Verification & Test Plan

### Priority Assessment
1. **Priority 1: Contrast Accessibility.** The text must meet standard WCAG contrast ratios in both modes.
2. **Priority 2: Zero FOUC.** The site must not flash white before turning dark on page reload.
3. **Priority 3: Markdown Integrity.** Ensure `marked.js` generated HTML inside assistant bubbles respects the CSS variables.

### Verification Checklist
- [ ] Toggle button swaps the data-theme attribute on `<html>` or `<body>`.
- [ ] `localStorage` correctly saves and restores the state across page reloads.
- [ ] OS-level preference (`prefers-color-scheme`) is respected upon first visit.
- [ ] Code blocks, citations, and tables remain clearly visible in dark mode.
- [ ] No server-side changes are required (pure frontend feature).