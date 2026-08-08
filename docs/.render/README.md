# Diagram render + verify

Renders the Mermaid diagrams in `../bean-architecture.html` to `../preview-all.png`
and asserts they actually rendered (right number of SVGs, zero error SVGs, and the
key responsibility/state labels present). It runs stock Mermaid v11 unmodified —
this is only a headless-Chromium harness, not a Mermaid extension.

## Usage

```sh
cd docs/.render
npm install            # mermaid@11 + playwright-core (gitignored)
node render.mjs        # writes ../preview-all.png, exits non-zero on any render error
```

Requires a Playwright Chromium already cached under
`~/Library/Caches/ms-playwright/`. Adjust the `EXE` path at the top of
`render.mjs` if your platform/version differs.

The source of truth for the diagrams is `../architecture-diagrams.md` (and the
same blocks mirrored in `../bean-architecture.html`); this dir just renders them.
