# Docs Guide

This directory documents only the retained Go reminder-assistant runtime.

## Authoring rules

- Run `pnpm docs:list` before doc work and after changing navigation.
- Every public page needs `title` and `summary` front matter.
- Use root-relative internal links without `.md` or `.mdx` suffixes.
- Keep examples generic: no personal hostnames, device names, absolute local
  paths, credentials, or secret values.
- Describe current source and verified behavior. Mark incomplete behavior as a
  limitation; do not document plans as shipped features.
- Do not restore upstream Node, plugin, provider, channel, CLI, UI, or
  localization pages as compatibility documentation.
- Update `docs/docs.json` whenever pages are added, renamed, or removed.

Long-lived private operator notes do not belong in this public directory.
