# LCTK visual identity

## Status

Accepted visual identity. The primary mark was selected on 2026-08-06 and the reusable vector system was derived from that approved concept.

## Meaning

The mark consists of two interlocking angular forms:

- the graphite form represents the stable boundary of a local project;
- the teal form represents an active, structured path through the codebase;
- the open negative space keeps the mark about traversal and tooling rather than a sealed lock, cloud, model, or specific editor.

The silhouette must remain calm, technical, and infrastructure-oriented. It must not acquire AI sparkles, a terminal prompt, circuit decoration, gradients, shadows, rounded badge containers, or additional internal detail.

## Canonical colors

| Role | Hex | Use |
|---|---|---|
| Graphite | `#31363D` | Primary boundary, wordmark, light-background monochrome |
| Teal | `#0B8197` | Primary active-path accent |
| Warm white | `#F7F7F4` | Reverse boundary and wordmark |
| Bright teal | `#21B7C5` | Reverse active-path accent |
| Dark surface | `#171B20` | Recommended dark presentation background, not part of transparent source assets |

No gradient is canonical. Raster exports contain antialiasing only; their interior fills remain the exact solid colors above.

## Variants

- `lctk-mark.svg` is the canonical two-color master for light or neutral backgrounds.
- `lctk-mark-reverse.svg` is the canonical mark for dark backgrounds.
- `lctk-mark-mono.svg` uses `currentColor` for single-ink reproduction.
- `lctk-mark-16.svg`, `lctk-mark-24.svg`, and `lctk-mark-32.svg` are optically adjusted masters for small raster targets.
- `lctk-wordmark-on-light.svg` and `lctk-wordmark-on-dark.svg` contain original outlined letterforms and have no font dependency.
- `lctk-lockup-on-light.svg` and `lctk-lockup-on-dark.svg` are the horizontal brand signatures.

## Spacing and minimum size

Keep clear space equal to at least one eighth of the mark width on every side. Do not place the primary mark below 16 pixels. Use the optical master that exactly matches a 16, 24, or 32 pixel target; larger raster exports use the canonical master. Do not place a horizontal lockup below 96 pixels wide.

The mark may be centered inside a square application-icon canvas, but it must not be stretched, rotated, outlined, cropped, recolored outside the canonical variants, or placed on a low-contrast background.

## Distribution inventory

The tracked [`assets/brand/`](../assets/brand/) directory contains:

- canonical SVG sources and the approved raster concept under `source/`;
- transparent primary and reverse PNG marks from 16 through 1024 pixels under `png/mark/`;
- 1024 by 256 pixel horizontal lockups under `png/lockup/`;
- a multi-frame Windows application icon under `windows/lctk.ico`;
- a multi-frame browser favicon under `web/favicon.ico`;
- a 1280 by 640 pixel GitHub social preview under `social/github-social-preview.png`.

The Windows icon contains 16, 20, 24, 32, 40, 48, 64, 96, 128, and 256 pixel frames. The favicon contains 16, 32, and 48 pixel frames.

Product screenshots and in-interface action or status icons are intentionally outside this identity package.

## Source integration

The Windows amd64 main packages under `cmd/lctk`, `cmd/lctk-launcher`, `cmd/lctk-setup`, and `cmd/lctk-local-setup` each carry the same `rsrc_windows_amd64.syso`. Go includes this package-local COFF resource automatically, so an ordinary next build receives the application icon without a release-workflow or build-script mutation. The resource was generated from `windows/lctk.ico` with `github.com/akavel/rsrc` v0.10.2 for amd64; its group-icon resource identifier is `1`.

The native setup and administrator window classes load their large and small icons from that same executable resource. Their visible header mark uses the already loaded large handle, keeping title bar, taskbar, Alt+Tab, and window content consistent without a loose runtime file.

The local OAuth waiting page embeds `source/lctk-lockup-on-light.svg` and serves the embedded `web/favicon.ico`. It performs no asset filesystem read and no remote request. All request-derived client, project, and callback values continue through `html/template` escaping; only the reviewed static SVG is admitted as trusted markup.
