---
summary: "CLI reference for `openclaw setup` (initialize config plus workspace, optionally run onboarding)"
read_when:
  - You're doing first-run setup without full CLI onboarding
  - You want to set the default workspace path
  - You need every flag and how setup decides between baseline and wizard mode
title: "Setup"
---

# `openclaw setup`

Initialize the baseline config and agent workspace. With any onboarding flag present, also runs the wizard.

<Note>
`openclaw setup` is for mutable config installs. In Nix mode (`OPENCLAW_NIX_MODE=1`) OpenClaw refuses setup writes because the config file is managed by Nix. Use the first-party [nix-openclaw Quick Start](https://github.com/openclaw/nix-openclaw#quick-start) or the equivalent source config for another Nix package.
</Note>

## Options

| Flag                     | Description                                                                                         |
| ------------------------ | --------------------------------------------------------------------------------------------------- |
| `--workspace <dir>`      | Agent workspace directory (default `~/.openclaw/workspace`; stored as `agents.defaults.workspace`). |
| `--wizard`               | Run interactive onboarding.                                                                         |
| `--non-interactive`      | Run onboarding without prompts.                                                                     |
| `--accept-risk`          | Acknowledge full-system agent access risk; required with `--non-interactive`.                       |
| `--mode <mode>`          | Onboarding mode: `local` or `remote`.                                                               |
| `--remote-url <url>`     | Remote Gateway WebSocket URL.                                                                       |
| `--remote-token <token>` | Remote Gateway token (optional).                                                                    |

### Wizard auto-trigger

`openclaw setup` runs the wizard when any of these flags are explicitly present, even without `--wizard`:

`--wizard`, `--non-interactive`, `--accept-risk`, `--mode`, `--remote-url`, `--remote-token`.

## Examples

```bash
openclaw setup
openclaw setup --workspace ~/.openclaw/workspace
openclaw setup --wizard
openclaw setup --non-interactive --accept-risk --mode remote --remote-url wss://gateway-host:18789 --remote-token <token>
```

## Notes

- Plain `openclaw setup` initializes config and workspace without running the full onboarding flow.
- After plain setup, run `openclaw onboard` for the full guided journey, `openclaw configure` for targeted changes, or `openclaw channels add` to add channel accounts.

## Related

- [CLI reference](/cli)
- [Onboarding (CLI)](/start/wizard)
- [Getting started](/start/getting-started)
- [Install overview](/install)
