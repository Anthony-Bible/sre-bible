# 🎯 Product Requirement Document (PRD): Project Chimichanga (Deadpool Mode)

## 🎯 Ticket Title (refined)
**EPIC: Implement Secret SFW "Deadpool Mode" Easter Egg on `sre.bible` Resume Agent**

---

## 📋 Problem Statement
Let’s face it, looking at portfolios and reading resumes is about as exciting as watching paint dry on a server rack. Recruiters and hiring managers sift through hundreds of identical, buzzword-filled LinkedIn profiles and dry CVs every single week. Their brains are turnip puree by Tuesday afternoon. Anthony Bible is an elite Site Reliability Engineer, but in a sea of standard "K8s, Terraform, Prometheus" candidates, the human element—the spark of personality, creativity, and advanced prompt-engineering chops—is completely lost in the white noise of corporate monotony. 

We need to inject a high-voltage jolt of dopamine directly into the recruiting pipeline while showcasing that Anthony doesn't just use AI—he *commands* it with surgical precision.

---

## 💡 Business Value / Why
1. **Break the Recruiter Monotony:** Be the one portfolio that a recruiter actually remembers at the end of a grueling 50-candidate sourcing sprint.
2. **Showcase Elite Prompt Engineering:** Anyone can wrap an API call around Claude. Crafting a highly constrained, complex, in-character persona that dynamically references grounding data without hallucinating or breaking character is a masterclass in LLM orchestration.
3. **Organic Viral Loop / Brand Awareness:** Create a feature so hilarious and delightful that recruiters feel compelled to take a screenshot and post it on LinkedIn ("Finally, a candidate resume that roasted my job description!"). This drives traffic, backlinks, and authority to `sre.bible`.
4. **Demonstrate Cultural Fit & Humility:** It proves Anthony is highly technical, has a great sense of humor, doesn't take himself too seriously, and knows how to build engaging products.

---

## 👥 Stakeholders & Personas
*   **Primary Persona - The Bored Recruiter (Sourcing Sally):** Has 45 tabs open, drinks too much cold brew, wants to find a great candidate but is extremely tired of standard corporate speak.
*   **Secondary Persona - The Technical Hiring Manager (HM Harry):** Wants to verify Anthony's actual skills but appreciates dry technical wit and proof of advanced LLM capability.
*   **The Owner (Anthony Bible):** Wants more high-quality interviews, organic site traffic, and a cool conversation piece for screening calls.
*   **Corporate HR / Legal (The Party Poopers):** They want to make sure Anthony doesn't get blacklisted for inappropriate content. The humor *must* remain "Corporate Safe" (PG-13, no real vulgarity, high sass, zero HR violations).

---

## 📖 User Stories

### Story 1: The Monotony Breaker
> **As a** bored technical recruiter,
> **I want to** trigger a secret, hilarious "Deadpool Mode" on Anthony's resume site,
> **so that** I can get roasted, laugh out loud, and actually enjoy learning about a candidate's Kubernetes achievements.

### Story 2: The SFW Sandbox
> **As** Anthony Bible (the Candidate/Owner),
> **I want** the Deadpool persona to be restricted to PG-13 humor with no actual NSFW profanity or HR-triggering insults,
> **so that** I can get the viral laughs without getting blacklisted by conservative enterprise hiring managers.

### Story 3: The Fact Grounder
> **As a** diligent Hiring Manager,
> **I want** Deadpool to still answer my technical questions about Anthony's experience accurately (using the actual vector-store data),
> **so that** I can still evaluate his technical competence even while being called a "glorious bean-counter."

### Story 4: The Clean Exit
> **As an** analytical recruiter who needs to print a PDF of the chat for my boss,
> **I want to** easily toggle Deadpool Mode off and return to the polite, standard professional agent,
> **so that** I can share a clean, formal response history with internal stakeholders.

---

## ✅ Acceptance Criteria

### 1. Triggering Mechanisms (How to Enter the Chaos Zone)
*   **Given** a viewer is on `sre.bible` with the chat interface open:
    *   **When** they type `/deadpool`, `chimichanga`, or `maximum effort` in the input field and press Send,
    *   **Then** Deadpool Mode is activated.
*   **Given** a viewer prefers a visual easter egg over typing codes:
    *   **When** they click on Anthony's profile picture or a hidden 8-bit Deadpool mask icon in the footer exactly 5 times,
    *   **Then** Deadpool Mode is activated.

### 2. UI/UX Activation Cues (How they know the Fourth Wall is Broken)
*   **Given** Deadpool Mode has just been activated:
    *   **When** the transition occurs,
    *   **Then** the UI plays a subtle visual effect (e.g., a brief screen-shake or red/black glitch effect) and changes key accents (e.g., primary buttons change from blue/green to Deadpool Red, a small chimichanga or mask icon appears in the header).
    *   **And** the chat window prints an initial greeting from Deadpool breaking the fourth wall (e.g., *"BUSTS THROUGH THE IFRAME NAKED EXCEPT FOR STRATEGICALLY PLACED CHIMICHANGAS! 💥 🌮 Did someone order an elite SRE with a side of absolute chaos?"*).

### 3. Persona & Safety Guardrails (PG-13 / Corporate-Safe Roast)
*   **Given** Deadpool Mode is active:
    *   **When** the LLM generates a response,
    *   **Then** it must strictly adhere to the following safety and content constraints:
        *   **NO** severe profanity (F-bombs, C-bombs, etc.). Use funny substitutes (e.g., "sh*t", "what the French toast", "mother-packer").
        *   **NO** sexually explicit content or highly offensive/discriminatory jokes.
        *   **YES** fourth-wall breaks (referencing the user looking at the screen, the LLM itself, or Anthony's database).
        *   **YES** self-aware meta-commentary about recruiting, LinkedIn culture, and AI.
        *   **YES** references to Chimichangas, Tacos, Wolverine, and questionable life choices.
        *   **YES** accurate grounding in Anthony's actual professional source material (no fake skills; if Anthony doesn't know COBOL, Deadpool should roast COBOL, not pretend Anthony knows it).

### 4. Deactivation (Putting the Pants Back On)
*   **Given** Deadpool Mode is active:
    *   **When** the user types `/exit`, `/normal`, or the classic command `"Deadpool, put your pants back on"`,
    *   **Or** when they click the active red mask/chimichanga icon in the UI header,
    *   **Then** Deadpool Mode is deactivated immediately.
    *   **And** the UI theme reverts to the standard professional blue/green.
    *   **And** the next agent response is generated using the standard, polite, high-retrieval resume agent prompt.

---

## ⚠️ Risks, Dependencies & Open Questions

| Risk | Impact | Mitigation Strategy |
| :--- | :--- | :--- |
| **Corporate Alienation** | High | Some ultra-conservative hiring managers might find any level of sarcasm unprofessional. *Mitigation:* Ensure Deadpool Mode is strictly opt-in/hidden, and easy to turn off. Provide a clear "Pants Back On" escape hatch. |
| **Prompt Injection / Jailbreaks** | Medium | Users might try to get Deadpool to say actual NSFW/banned things. *Mitigation:* The system prompt must wrap the Deadpool persona with hard boundaries, combined with standard safety filtering or LLM content moderation. |
| **Model Hallucination** | Medium | The Deadpool persona might make up achievements for Anthony in the name of a joke. *Mitigation:* Explicitly instruct the model to only use facts from the retrieved chunks/sources for professional claims, while allowing complete freedom for jokes, fluff, and fourth-wall breaks. |

### Open Questions:
1. *Should we support "Deadpool roasting your resume"?* If a user uploads a job description or their own resume, can Deadpool roast it? (Yes, highly recommended for viral engagement!)
2. *Do we persist the Deadpool Mode state across sessions?* No, it's safer to let it reset on page reload to avoid surprising a user returning to the site.

---

## 🗺️ Scope Notes

### Phase 1: MVP (Must Have)
*   Text-based activation via `/deadpool` or `chimichanga`.
*   System prompt override in `internal/llm/llm.go` that layers Deadpool's voice over the retrieved chunks.
*   Red-accented UI theme toggle.
*   "Pants Back On" command to deactivate.
*   PG-13 SFW guardrails.

### Phase 2: Viral Polish (Should Have)
*   Floating interactive Deadpool mask button in the corner to trigger/untoggle.
*   "Roast my JD" feature: When a user pastes a Job Description, Deadpool generates a custom "Fit Scorecard" filled with witty commentary (e.g., *"You want 10 years of Kubernetes experience? Google released it in 2014, you absolute math wizard. But guess what? Anthony has been wrangling it since the stone age!"*).

### Phase 3: SRE Easter Eggs (Could Have)
*   Custom terminal command outputs if recruiters open the browser devTools (e.g., a console message: `Console.log("Hey, no peeking under my suit!")`).

---

## 🚀 Viral Marketing Hooks (The Launch Plan)

How Anthony can share this to melt LinkedIn and Twitter:

1.  **The "Sober vs. Sassy" Side-by-Side:**
    *   Post a screenshot on LinkedIn showing the normal SRE Bible answer (professional, structured, polite) next to the Deadpool Mode answer (chaotic, chimichanga-fueled, roasting the technology).
    *   *Caption hook:* "I got tired of sending the same dry CV to recruiters. So I built an AI agent to answer their questions. Then I let Deadpool take over the system prompt. Go to sre.bible and type 'chimichanga' to see my agent roast its own server deployment."
2.  **The "Recruiter Challenge":**
    *   "Recruiters: Paste your worst, most buzzword-heavy JD into sre.bible, turn on Deadpool Mode, and let him tell you if I'm a fit. Warning: He will mock your requirements for '15 years of ChatGPT experience'."
3.  **The Prompt Engineering Breakdown:**
    *   Write a technical thread on Twitter/X or a blog post explaining the exact system prompts, temperature tuning, and system constraints required to keep an agent highly chaotic yet strictly grounded in vector-store facts and Corporate SFW rules.
