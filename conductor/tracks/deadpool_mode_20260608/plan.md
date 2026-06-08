# Implementation Plan: Implement Secret 'Deadpool Mode' Easter Egg

## Phase 1: Foundation & LLM Prompting
- [ ] Task: Write Tests for Deadpool prompt routing and state handling.
- [ ] Task: Create the Deadpool system prompt with strict PG-13 rails and tech-roast instructions.
- [ ] Task: Update the backend Go chat handler to accept a `deadpool_mode` flag and route to the new prompt.
- [ ] Task: Conductor - User Manual Verification 'Phase 1: Foundation & LLM Prompting' (Protocol in workflow.md)

## Phase 2: Frontend Trigger & State Management
- [ ] Task: Write Tests (if applicable/e2e) for frontend state management.
- [ ] Task: Implement keyword detection (`/deadpool`, `chimichanga`, `maximum effort`) in the chat input JS.
- [ ] Task: Implement the hidden UI click-trigger (e.g., 5 clicks on a footer icon).
- [ ] Task: Conductor - User Manual Verification 'Phase 2: Frontend Trigger & State Management' (Protocol in workflow.md)

## Phase 3: Visuals, Transitions & Deactivation
- [ ] Task: Write Tests (if applicable/e2e) for deactivation logic.
- [ ] Task: Add CSS/JS for the "Deadpool Red" visual transition and screen shake effect.
- [ ] Task: Implement the deactivation commands (`/exit`, `/normal`, `Deadpool, put your pants back on`) and UI toggle to revert state and styling.
- [ ] Task: Conductor - User Manual Verification 'Phase 3: Visuals, Transitions & Deactivation' (Protocol in workflow.md)