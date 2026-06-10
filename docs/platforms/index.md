---
summary: "Platform support overview (Docker-first Gateway)"
read_when:
  - Looking for OS support or install paths
  - Deciding where to run the Gateway
title: "Platforms"
---

This slim fork is **Docker-first** and runs the Gateway on Linux. OpenClaw core
is written in TypeScript with **Node as the runtime**. The recommended setup is
the Docker image; see [Docker](/install/docker).

There are no companion desktop or mobile apps in this fork (Windows, macOS, iOS,
and Android apps are not supported).

## Run it

- Docker (recommended): [Docker](/install/docker)
- Linux host: [Linux](/platforms/linux)

## VPS and hosting

- VPS hub: [VPS hosting](/vps)
- Fly.io: [Fly.io](/install/fly)
- Hetzner (Docker): [Hetzner](/install/hetzner)
- GCP (Compute Engine): [GCP](/install/gcp)
- Azure (Linux VM): [Azure](/install/azure)
- exe.dev (VM + HTTPS proxy): [exe.dev](/install/exe-dev)
- EasyRunner (Podman + Caddy): [EasyRunner](/platforms/easyrunner)
- DigitalOcean: [DigitalOcean](/platforms/digitalocean)
- Oracle Cloud: [Oracle](/platforms/oracle)
- Raspberry Pi: [Raspberry Pi](/platforms/raspberry-pi)

## Gateway service install (CLI)

Use one of these:

- Wizard (recommended): `openclaw onboard --install-daemon`
- Direct: `openclaw gateway install`
- Repair/migrate: `openclaw doctor` (offers to install or fix the service)

On Linux/WSL2 the service target is a systemd user service
(`openclaw-gateway[-<profile>].service`).

## Related

- [Install overview](/install)
- [Gateway runbook](/gateway)
- [Gateway configuration](/gateway/configuration)
