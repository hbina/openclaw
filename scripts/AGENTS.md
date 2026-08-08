# Script Intent

Scripts exist to make recurring repository operations reproducible, safe, and
easy to audit. They should encode a deliberate project seam, not hide product
behavior or replace an understandable command with another layer of ceremony.

## Why Wrappers Exist

A curated wrapper is valuable when it centralizes environment setup, preserves
repository-specific policy, or prevents different callers from implementing
the same fragile workflow differently. Once such a seam exists, extending it
is preferable to copying its rules into hooks, CI jobs, or ad hoc commands;
duplication allows those paths to drift while appearing equivalent.

Wrapper mechanics and available entrypoints are discoverable from the scripts
themselves. This file should retain the reason for a seam, not a catalog of
current filenames or invocation syntax.

## Resource and Concurrency Safety

Heavy-check coordination exists to protect developer machines and concurrent
work from redundant resource-intensive jobs. Bypassing that coordination to
make one local invocation finish sooner transfers cost and instability to
other work. Exceptions should remain narrow, explicit, and supported by tests
that demonstrate why they are safe.

Scripts must respect the repository’s broader safety model: preserve unrelated
work, fail visibly on partial setup, avoid destructive expansion of paths, and
never reveal credentials. Automation magnifies small mistakes, so target
selection and failure handling should be easier to audit than the manual
operation it replaces.

## Generated Artifacts

Generation and verification are two halves of one contract. A generator
without a matching consistency check allows committed output to drift from its
source; a check without an authoritative generator leaves maintainers guessing
how to repair it. Keep those responsibilities aligned and make the generated
boundary explicit.

## Scope

Add or expand a script when it solves a recurring repository problem with a
stable interface. One-off migration detail, production application behavior,
and policy that belongs at repository scope should not be hidden here. The
root `AGENTS.md` owns product-wide intent and verification expectations; this
file adds only the rationale specific to automation.
