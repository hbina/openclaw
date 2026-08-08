# Documentation Intent

This directory serves operators and maintainers of the retained Go
reminder-assistant runtime. Its purpose is to explain user-visible behavior,
operational consequences, and the reasoning behind important constraints.

## What the Documentation Should Preserve

Documentation captures why a behavior exists, what outcome it gives the owner,
which trade-offs were chosen, and where the boundary of support lies. Source
code already describes implementation mechanics; avoid narrating functions,
packages, or control flow unless an operator needs that model to make a safe
decision.

Describe only behavior supported by the current Go runtime and verified at the
appropriate boundary. Clearly mark limitations and unresolved work. Plans must
not read like shipped features because operators make deployment and data
decisions from these pages.

The public documentation intentionally excludes deleted upstream Node,
provider, plugin, channel, CLI, UI, and localization surfaces. Restoring those
pages as compatibility documentation would imply a support commitment this
fork does not make.

## Audience and Safety

Examples should be generic and portable. Personal hostnames, device names,
absolute local paths, credentials, tokens, and private operator notes would
turn public guidance into a disclosure risk or make it misleading for another
deployment.

Operational commands belong in documentation when an operator genuinely needs
them to deploy, recover, verify, or diagnose the product. Explain the purpose,
expected evidence, and relevant risk rather than presenting unexplained command
sequences. Ephemeral container IDs, one-machine observations, and temporary
test details should not become durable documentation.

Navigation and metadata should help readers discover the supported product,
but they are not the subject of this guidance. Let the documentation tooling
and repository configuration express their mechanics instead of duplicating
them here.

## Truthfulness Standard

Prefer an explicit limitation over an optimistic inference. Documentation
should agree with source, tests, and observed behavior, especially for local
model interactions, persistence, reminder delivery, and recovery. When those
sources disagree, resolve the discrepancy or identify it; do not select the
most favorable account.

Long-lived private operator notes do not belong in this public directory.
