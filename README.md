# sc-plugin-cloudflare

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

The **Cloudflare registrar driver for [Simple Container](https://github.com/simple-container-com/api)**, extracted from the SC monolith's `pkg/clouds/{cloudflare,pulumi/cloudflare}` subtrees as the **first out-of-tree sc-plugin** per the [`sc-plugin-architecture`](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-10-24/sc-plugin-architecture) design.

A plugin binary lets SC drive Cloudflare DNS + Workers edge from a stack without SC core having to vendor the (~180 MB) `pulumi-cloudflare` SDK. The plugin talks to SC core's running Pulumi engine over the SC plugin bus, registers resources against the SAME monitor, and shows up in state exactly as the in-tree driver did.

## Table of contents

- [What this plugin does](#what-this-plugin-does)
- [Install](#install)
- [Configure in `server.yaml`](#configure-in-serveryaml)
- [Build from source](#build-from-source)
- [Repository layout](#repository-layout)
- [Protocol](#protocol)
- [Compatibility](#compatibility)
- [Live-smoked](#live-smoked)
- [License](#license)

## What this plugin does

Everything the pre-split `pkg/clouds/pulumi/cloudflare/` subtree did:

- **Zone lookup** — resolves the stack's `zoneName` to a Cloudflare zone ID.
- **DNS records** — bulk at `provision` time via the `dnsRecords:` list; per-record on demand via `NewDnsRecord` for drivers that register endpoint DNS at resource-provisioning time.
- **Host-header override Workers** — the full worker-script-plus-route pattern SC uses for VM-compose and k3s-cloudrun to put a custom domain in front of an origin that doesn't speak that hostname.
- **Domain-for-endpoint shortcut** — the common "proxied CNAME + override worker" combo in one call.

What it does NOT do (yet):

- **Streaming per-record output progress** — plugin currently returns URNs synchronously; progress bars don't tick during long creates. Follow-up with the Fingerprint RPC.
- **Cloudflare resources beyond the registrar surface** — if a future SC feature needs Pages, R2, Durable Objects, etc, add them as new resource types the plugin advertises in Schema.

## Install

### Download a release binary

Pre-built binaries for each platform are attached to each GitHub Release:

```sh
# Linux amd64 example — adjust URL for your platform / version.
curl -sSLO https://github.com/simple-container-com/sc-plugin-cloudflare/releases/download/v1.0.0/sc-plugin-cloudflare-linux-amd64
curl -sSLO https://github.com/simple-container-com/sc-plugin-cloudflare/releases/download/v1.0.0/sha256sums.txt
sha256sum -c sha256sums.txt --ignore-missing  # must say "OK"
chmod +x sc-plugin-cloudflare-linux-amd64
mv sc-plugin-cloudflare-linux-amd64 /opt/sc-plugins/sc-plugin-cloudflare
```

### Build from source

See [Build from source](#build-from-source) below.

## Configure in `server.yaml`

Add the plugin to the `plugins:` block and declare the registrar under `resources.registrar:` with `type: cloudflare`:

```yaml
schemaVersion: 1.0

provisioner:
  type: pulumi
  config:
    organization: your-org
    state-storage: { type: s3-bucket, config: {...} }
    secrets-provider: { type: awskms, config: {...} }

plugins:
  cloudflare:
    path: /opt/sc-plugins/sc-plugin-cloudflare
    sha256: <sha256sum of the binary>   # optional but strongly recommended

resources:
  registrar:
    type: cloudflare
    config:
      zoneName: your-zone.example.com   # the Cloudflare zone this stack serves
      accountId: ${env:CF_ACCOUNT_ID}   # the account that owns the zone
      credentials: ${secret:CF_API_TOKEN}
      dnsRecords:
        - name: www
          type: A
          value: 203.0.113.42
          proxied: true

  resources:
    staging:
      resources: {}
```

The `credentials` value is a Cloudflare API token (not a legacy global API key) with at least:
- `Zone:Zone:Read` on the specific zone (for `LookupZone`),
- `Zone:DNS:Edit` on the specific zone (for record / worker-route creation),
- `Account:Workers Scripts:Edit` on the account (for worker-script creation).

The `accountId` field is required and must match the account the zone lives in — the plugin pins `LookupZone` with `Account.Id` + `Match: "all"` to resolve same-named zones deterministically.

### Zone-vs-account authority

Multiple Cloudflare accounts can own a zone with the same name (e.g. a migrated zone). This plugin fails fast if `LookupZone` returns multiple results for `(zoneName, accountId)`. If you see "failed to find exactly one result: N found", you have N zones in that account with the same name — delete or rename the duplicates in Cloudflare's dashboard.

## Build from source

```sh
git clone https://github.com/simple-container-com/sc-plugin-cloudflare.git
cd sc-plugin-cloudflare/plugin
go build -o /tmp/sc-plugin-cloudflare ./cmd/sc-plugin-cloudflare
```

Resulting binary is ~178 MB (vendors `pulumi-cloudflare` SDK v6.17.0). Peak RSS during build is ~2 GB — well below SC core's.

Requires Go 1.23+. The build does **not** need the SC core repo cloned; dependencies resolve through `go.mod`.

## Repository layout

```
sc-plugin-cloudflare/
  api/                              # Go module — config structs for SC core
    cloudflare.go                   # AuthConfig + RegistrarConfig
    init.go                         # api.RegisterProviderConfig for "cloudflare"
    cloudflare_test.go              # trivial unit tests
    go.mod                          # imports: SC's pkg/api only. NO pulumi-cloudflare.
  plugin/                           # Go module — the plugin binary
    cmd/sc-plugin-cloudflare/
      main.go                       # SCCloudPluginServer implementation
    internal/
      registrar.go                  # lifted driver code — the Pulumi registrar
      ips.go                        # Cloudflare edge IP range lookup
    go.mod                          # imports: pulumi-cloudflare SDK + SC plugin runtime
  LICENSE                           # MIT
  CODEOWNERS
  CLAUDE.md                         # notes for AI coding agents
  README.md                         # this file
  .github/workflows/                # (TODO) release automation
```

**Two Go modules.** SC core imports `/api` at build time to register the "cloudflare" type in its config-tier map and generate schemas; the `/plugin` module and its heavy `pulumi-cloudflare` SDK dependency never cross that boundary. Same pattern [D3](https://github.com/simple-container-com/forge/blob/main/docs/design/2026-10-24/sc-plugin-architecture/06-decisions.md#d3) in the design doc fixes for the whole fleet of future plugin repos.

## Protocol

The plugin advertises on the SC plugin bus v1:

```proto
SchemaResponse {
  plugin_version: "cloudflare-0.0.1"
  registrar_types: ["cloudflare"]
  pulumi_plugins: [{ package: "cloudflare", version: "6.17.0" }]
}
```

Implements these RPCs from [`SCCloudPlugin`](https://github.com/simple-container-com/api/blob/main/pkg/plugin/proto/v1/plugin.proto):

| RPC | What it does |
|---|---|
| `Schema` | Advertise the surface above. |
| `ProvisionRegistrar` | Spin up the Cloudflare Pulumi provider resource + look up the configured zone. Return `main_domain`. |
| `NewDnsRecord` | Register one DNS record. Called by other SC drivers that need endpoint DNS. |
| `NewOverrideHeaderRule` | Register a Worker script + Workers route for host-header override. Used by `vm-compose` + `k3s-cloudrun`. |
| `ProvisionDomainForEndpoint` | Combine `NewDnsRecord` (proxied CNAME) + `NewOverrideHeaderRule` for a cloud endpoint. |
| `PreDestroy` | No-op for this driver — Pulumi's own cloudflare provider handles cleanup. |

Each RPC opens a short-lived `pulumi.Context` against `req.Header.MonitorAddress` and reuses the lifted `internal/registrar.go` code unchanged. The plugin never writes to SC's Pulumi state directly — resources are registered against SC core's monitor via the MonitorProxy.

## Compatibility

| sc-plugin-cloudflare | SC core (`simple-container-com/api`) | Pulumi SDK | pulumi-cloudflare |
|---|---|---|---|
| v0.0.x (initial) | main after PR #482 lands | sdk/v3 v3.245.0 | v6.17.0 |

The plugin binary vendors its own `pulumi-cloudflare` SDK; SC core's version doesn't matter after this plugin is pinned. SC core's `pulumi/sdk/v3` version MUST match the plugin's — a mismatch breaks the shared monitor gRPC protocol. Pin both via the plugin's `/plugin/go.mod` and SC core's `go.mod`.

## Live-smoked

2026-10-24, locally built sc against `ai-taskmanager.space` (Cloudflare zone `e41f418f1202c8a31a4cea83969975cb`):

```
$ sc provision -s smoke-cf --skip-preview
+ pulumi:pulumi:Stack: (create)
+ pulumi:providers:cloudflare: (create)
    apiToken  : [secret]
    version   : "6.17.0"
Resources: + 2 created, Duration: 1s

$ echo Y | sc destroy -s smoke-cf --parent --skip-preview --skip-refresh
Removed stack: all resources have been destroyed
```

The provider resource at
`urn:pulumi:smoke-cf::sc-plugin-smoke::pulumi:providers:cloudflare::<account-id>`
was created by the plugin subprocess talking to SC core's Pulumi monitor via the MonitorProxy; SC core's `pkg/clouds/pulumi/cloudflare` subtree was not engaged. Full write-up: [Addendum E in `04-no-destroy-invariant.md`](https://github.com/simple-container-com/forge/blob/main/docs/design/2026-10-24/sc-plugin-architecture/04-no-destroy-invariant.md).

## Development

See [CLAUDE.md](./CLAUDE.md) for notes to AI coding agents (and humans) — architecture context, the two-module shape, the four bugs the extraction smoke caught + how the code defends against them, and the live-smoke pattern.

Commits must carry a DCO sign-off (`git commit -s`). Do not add `Co-Authored-By` trailers.

## License

[MIT](./LICENSE).
