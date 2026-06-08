# Track Specification: Implement Secret 'Deadpool Mode' Easter Egg

## Objective
Add a hidden, interactive "Deadpool Mode" to the `sre.bible` resume agent to showcase advanced prompt engineering, provide a memorable experience for recruiters, and demonstrate cultural fit through PG-13 tech-focused humor.

## Scope
1.  **Trigger Mechanism:** Add logic to detect activation keywords (`/deadpool`, `chimichanga`, `maximum effort`) in the chat input or repeated clicks on a hidden UI element.
2.  **State Management:** Track the activation state of the easter egg in the frontend and pass it to the backend.
3.  **UI/UX Updates:** Implement visual transitions (screen shake, color palette shift to Deadpool Red) and swap the agent avatar when active.
4.  **Prompt Engineering & Routing:** Create a new system prompt for the Deadpool persona. Route requests to this prompt when the mode is active. Ensure the AI retains facts from the vector store but adopts the chaotic, fourth-wall-breaking persona.
5.  **Deactivation Mechanism:** Allow users to exit the mode via commands (`/exit`, `/normal`, `Deadpool, put your pants back on`) or a UI toggle.

## Architecture & Integration
-   **Frontend (Templates/JS):** Update `index.html` and associated scripts to handle the triggers, state, and visual transitions.
-   **Backend (Go):** Update the chat handler to accept the state flag and route to the appropriate Anthropic/GenAI prompt structure.
-   **LLM Prompts:** Store the new Deadpool system prompt, ensuring strict PG-13 safety rails while allowing tech roasts.