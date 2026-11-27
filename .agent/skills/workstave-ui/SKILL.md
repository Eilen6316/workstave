---
name: workstave-ui
description: >-
  Design, implement, refine, and review Workstave desktop UI. Use for frontend pages, components, layouts, styles, navigation, menus, visual states, accessibility, interaction behavior, or UI refactoring and review. Follow Workstave's work-centric product principles, minimalist visual language, native desktop interaction conventions, and rendered-interface verification workflow. Apply to design proposals and implementation tasks; do not use for unrelated backend-only work.
---

# Workstave UI

Use this guide when designing Workstave desktop UI. Treat it as a visual and interaction reference; application behavior remains defined by code, tests, README, and package contracts. Visual simplicity must never override functional correctness or operational clarity.

## Workflow

1. Inspect relevant application code, contracts, existing components, and the current UI before making design decisions. Respect project instructions and the current implementation scope.
2. Identify the user's primary intent, information hierarchy, operational states, and required controls.
3. Reuse existing components, design tokens, and interaction patterns where appropriate. Do not introduce a new design system or fixed page structure without a demonstrated need.
4. Design the smallest coherent UI that satisfies the actual requirements and the principles below. For implementation requests, implement it; for design-only requests, deliver a proposal without changing application code; for review requests, identify issues and propose corrections without editing unless requested.
5. For implementation or review of existing UI, render the relevant interface in a browser or the actual desktop application when available. Inspect appearance and interaction behavior using the rendering verification criteria below. Run relevant existing frontend checks and tests when applicable.
6. Correct observed layout, styling, accessibility, and interaction issues within the requested implementation scope. Preserve application behavior and existing contracts.
7. Report relevant changes, verification results, and any checks that could not be performed. Do not claim visual correctness without inspecting the rendered result, or claim implementation or testing for a design-only proposal.

## Product design principles

- Design Workstave as a native, work-centric AI workspace, not a chatbot, dashboard, or Agent management platform.
- Organize the primary experience around Work, independent Conversations, and shared Work State.
- Prioritize work objectives, progress, decisions, deliverables, and matters requiring user attention. Reveal Agent, Session, Run, Harness, and execution logs progressively.
- Automate coordination rather than exposing its complexity to the user.
- Keep essential controls, approvals, risks, and unresolved states visible and accessible. Never sacrifice operational clarity for visual minimalism.
- Represent operational states according to their actual domain semantics. Distinguish request acceptance from execution, execution completion from task acceptance, pausing new dispatch from stopping active execution, cancellation requests from confirmed cancellation, and unknown outcomes from failures.
- Expose only capabilities supported by the current implementation. Do not invent product behavior or simulate unsupported functionality.

## Layout and information

- Do not nest cards or put large boxes around smaller boxes. Avoid unnecessary divider lines, borders, and decorative containers.
- Do not add unnecessary slogans, subtitles, secondary labels, or explanatory copy. Retain essential instructions, status information, errors, and risk disclosures.
- Show hierarchy through typography, alignment, and spacing rather than explanatory text.
- Use progressive disclosure to present information and controls according to their relevance to the current task.
- Maintain stable layouts during streaming responses, background execution, and contextual interactions.

## Visual style, color, and whitespace

- Use the visual restraint of the Apple website and the functional clarity of native desktop productivity applications as references.
- Keep about 90% of the interface neutral in black, white, and gray. Use one accent color for primary actions or key data. Reserve semantic colors for meaningful operational states.
- Do not use gradients, highly saturated or neon colors, or heavy card shadows.
- Use darker, heavier text for headings and lighter, regular-weight text for body copy.
- Use generous whitespace in reading and conversation areas, and compact, consistent spacing in navigation, task lists, and execution views.
- Leave deliberate empty space instead of filling every area. Balance visual simplicity with information density and operational efficiency.

## Typography, grid, and icons

- Use Geist throughout.
- Use 18px, weight 600 or 700, as the default section heading style. Use 16px or 14px, weight 400 or 500, for body text. Adjust typography only when required by information hierarchy.
- Use `#181818` for primary text and `#6A6A6A` for secondary text on light surfaces. Maintain readable contrast in all supported themes and interaction states.
- Use multiples of 4px for all spacing.
- Choose radii from 8px to 40px according to component size and hierarchy. Use smaller radii for small controls and larger radii for large surfaces and dialogs. Keep equivalent components consistent.
- Use icons from [Reicon](https://reicon.dev/docs). Set the icon glyph to 18px with a 1.5px stroke inside a transparent 24px by 24px frame.
- Use icons consistently and purposefully. Avoid decorative icons and redundant visual elements.

## Interaction and desktop menus

- Show controls according to user intent. Reveal secondary actions on hover, keyboard focus, or relevant interaction without causing layout shifts. Keep essential controls visible and accessible.
- Support keyboard navigation, visible focus states, predictable scrolling, and resizable desktop windows. Prefer native desktop interaction conventions over website conventions.
- Keep interactions responsive during background execution. Deterministic controls must not be blocked by Agent reasoning.
- Provide clear feedback for user actions, asynchronous operations, errors, and unresolved states. Do not rely on color alone to communicate meaning.
- Multi-level desktop menus must implement an opening delay, a closing delay, and a safe triangle. When the pointer moves from parent menu A toward submenu B, form a triangle from the pointer's departure position toward the near upper and lower corners of submenu B. While the pointer remains within that triangle and moves toward B, keep B open and delay switching to another menu item along the way. Apply a bounded delay so menus remain responsive.
- Keep motion subtle, purposeful, and brief. Avoid animations that interrupt workflows or delay interaction.

## Rendering verification

- Judge the result by its actual browser-rendered appearance, not computed style values alone. Inspect the actual desktop window as well when native behavior matters and the environment permits it.
- After implementation, inspect the rendered screen for alignment, wrapping, whitespace, contrast, clipping, overflow, and occlusion.
- Verify the interface across representative window sizes, content lengths, and supported interaction states.
- Inspect relevant loading, empty, running, queued, completed, error, disconnected, and unknown states according to the actual application behavior.
- Verify keyboard navigation, focus visibility, contextual controls, and submenu pointer behavior where relevant.
- Correct observed visual and interaction problems before considering the implementation complete. If rendering or interaction checks are unavailable, explicitly identify the unverified scope.
