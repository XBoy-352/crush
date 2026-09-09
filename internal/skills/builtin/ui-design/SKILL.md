---
name: ui-design
description: Guidelines and design system patterns for terminal user interfaces (Bubble Tea, Lip Gloss, ultraviolet) and web UI design, tokenized styling, layout architectures, and visual state progressions.
---

# UI Design System & Prototyping Guide

This skill provides best practices for designing user interfaces, both for terminal applications (using Charm's Bubble Tea, Lip Gloss, and ultraviolet frameworks) and web interfaces.

---

## 1. Token-Driven Design System

Never hardcode raw colors (such as `#FF00FF` or direct ANSI indices) across individual components. Always build on a structured token palette:

### **Semantic Token Architecture:**
- **Primary / Accent**: Brand identifiers, active focus borders, interactive selections.
- **Secondary / Muted**: Inactive borders, metadata subtitles, subtle separators.
- **Foreground Base (`fgBase`)**: Default body text with high readability.
- **Background Base (`bgBase`)**: Main canvas or dialog backdrop.
- **Status Indicators**: Semantic tokens for `Success`, `Warning`, `Error`, and `Info`.

### **Terminal Theming (Lip Gloss & QuickStyle):**
```go
// Define styles strictly using semantic theme tokens
type Styles struct {
    BorderActive   lipgloss.Style
    BorderInactive lipgloss.Style
    BadgeSuccess   lipgloss.Style
    TextDim        lipgloss.Style
}
```

---

## 2. Complete State Coverage

Every interactive component or page must be designed and handled for all 5 core UI states:

1. **Idle / Empty State**: Helpful onboarding text, clear call-to-action, no broken alignments.
2. **Loading / In-Flight State**: Non-blocking spinners, progress indicators, disabled inputs to prevent double-submit.
3. **Active / Focused State**: Clear visual differentiation (focused borders, inverted pill colors, cursor positioning).
4. **Success State**: Brief confirmation toasts, updated counters, clean transitions.
5. **Error State**: Actionable error messages, non-destructive recovery, retaining user input.

---

## 3. Terminal UI (TUI) Layout Architecture

When designing terminal interfaces with Bubble Tea and Lip Gloss:
- **Responsive Width & Height**: Always handle window resize messages (`tea.WindowSizeMsg`) and clamp boundaries gracefully.
- **Off-Thread IO**: Never perform synchronous disk, database, or network IO in the `Update` or `View` loop. Dispatch tasks as `tea.Cmd`.
- **Text Truncation & Wrapping**: Use ANSI-aware truncation (`ansi.Truncate`) and width measurements (`ansi.StringWidth`) to prevent line-wrapping artifacts.

---

## 4. Visual State Carousels

When presenting UI mockups or iterative design options, present states sequentially using Markdown carousels:

````markdown
````carousel
### State 1: Idle View
┌──────────────────────────────┐
│  Press [Enter] to Search     │
└──────────────────────────────┘
<!-- slide -->
### State 2: Loading / Search in Progress
┌──────────────────────────────┐
│  ⠋ Searching repositories... │
└──────────────────────────────┘
<!-- slide -->
### State 3: Results Found
┌──────────────────────────────┐
│  ✓ 3 items matched           │
│  • main.go                   │
│  • config.go                 │
└──────────────────────────────┘
````
````
