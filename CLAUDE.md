# sc-plugin-cloudflare — notes for AI coding agents

This is the first out-of-tree Simple Container cloud driver. It implements the
Cloudflare registrar surface of SC via the plugin-bus protocol defined in
[`simple-container-com/api/pkg/plugin/proto/v1`](https://github.com/simple-container-com/api/tree/main/pkg/plugin/proto/v1).

If you don't know what the SC plugin architecture is, read these first in
order:
- [`forge/docs/design/2026-10-24/sc-plugin-architecture/README.md`](https://github.com/simple-container-com/forge/blob/main/docs/design/2026-10-24/sc-plugin-architecture/README.md) — the overview.
- [`forge/docs/design/2026-10-24/sc-plugin-architecture/03-architecture.md`](https://github.com/simple-container-com/forge/blob/main/docs/design/2026-10-24/sc-plugin-architecture/03-architecture.md) §3 — the two gRPC conversations (SC plugin bus + Pulumi Resource Monitor).
- [`forge/docs/design/2026-10-24/sc-plugin-architecture/04-no-destroy-invariant.md`](https://github.com/simple-container-com/forge/blob/main/docs/design/2026-10-24/sc-plugin-architecture/04-no-destroy-invariant.md) Addendum E — the live extraction evidence for this plugin.
- [`api/pkg/plugin/README.md`](https://github.com/simple-container-com/api/blob/main/pkg/plugin/README.md) — the plugin-author surface guide with the 9-step extraction checklist this repo was built against.

## Package layout — two Go modules, one repo

```
sc-plugin-cloudflare/
  api/              one Go module — the config-tier structs. SC core imports this.
    cloudflare.go   AuthConfig + RegistrarConfig (RegistrarType const).
    init.go         api.RegisterProviderConfig for "cloudflare".
    go.mod          deps: SC's pkg/api only. NO pulumi-cloudflare SDK.
  plugin/           another Go module — the plugin binary. SC core NEVER imports this.
    cmd/sc-plugin-cloudflare/main.go  the plugin's main(): plugin.Serve + SCCloudPluginServer impl.
    internal/
      registrar.go  the actual Pulumi driver — lifted verbatim from
                    api's pre-split pkg/clouds/pulumi/cloudflare/registrar.go.
      ips.go        Cloudflare edge IP range lookup.
    go.mod          deps: pulumi-cloudflare SDK + pulumi/sdk/v3 + hashicorp/go-plugin
                    (via SC's pkg/plugin runtime).
```

**Why two modules.** SC core imports `/api` at build time (blank-imports from
the plugins block flow) to populate the schema registry. The `/plugin` module
would drag in `pulumi-cloudflare` SDK (~180 MB of generated Go) if SC core
touched it, defeating the "small sc core binary" goal of the plugin
architecture.

## How a request flows

```
operator writes server.yaml:        plugins: { cloudflare: { path: ... } }
                                    resources.registrar: { type: cloudflare, config: { zoneName, accountId, credentials } }

sc provision                        loads sc-plugin-cloudflare binary
  → preEnginePluginBundle           calls Schema() → sees registrar_types=["cloudflare"]
                                    registers a sidecar in pApi.RegistrarFuncByType["cloudflare"]

provisionProgram inline RunFunc     calls initRegistrar
  → RegistrarFuncByType["cloudflare"] sidecar  → serializes config via JSON
                                                → cli.ProvisionRegistrar(req)
                                                     plugin opens pulumi.NewContext against
                                                     req.Header.MonitorAddress (MonitorProxy
                                                     forwards to SC core's engine)
                                                   plugin calls cfInternal.Registrar(pctx, …)
                                                   which registers pulumi:providers:cloudflare
                                                   via ctx.RegisterResource — hits MonitorProxy
                                                   → forwards to real monitor → engine creates it

For NewDnsRecord / NewOverrideHeaderRule / ProvisionDomainForEndpoint, same shape
one per call, keyed by the registrar handle the plugin cached from ProvisionRegistrar.
```

The plugin never writes directly to SC's Pulumi state. All resources are
registered against **SC core's monitor** via the proxy — single Pulumi
session, exactly as the architecture design requires.

## Building

```sh
# /api — nothing to build; it's a types-only module. go vet enough.
cd api && go vet ./...

# /plugin — build the binary.
cd plugin && go build -o /tmp/sc-plugin-cloudflare ./cmd/sc-plugin-cloudflare
# Resulting binary: ~178 MB. Vendors pulumi-cloudflare SDK.
```

**Do not try to build /plugin from the parent dir with `go build ./...`.** The
two modules are deliberately separate; `go` doesn't recurse into siblings.

## Live-smoke pattern (what the extraction smoke runs)

```sh
# 1. Build the plugin binary from /plugin.
# 2. Place it where the fixture's server.yaml points (plugins.cloudflare.path).
# 3. Build sc from the api repo (needs ~13 GB RAM to link).
# 4. Write a fixture with cloudflare registrar + account id + token (the
#    account must own the zone).
# 5. sc provision -s <stack-name> --skip-preview
#    Expect: pulumi:providers:cloudflare created, main_domain returned.
# 6. sc destroy -s <stack-name> --parent --skip-preview --skip-refresh
#    (destroy is interactive; pipe Y via echo.)
```

The 2026-10-24 extraction smoke used `ai-taskmanager.space` as a scratch
zone. Any Cloudflare zone the token has account-level access to works —
pick one that is unique within its account to avoid the `LookupZone`
ambiguity the smoke caught (see §Four bugs below).

## Four bugs the extraction smoke caught — code is defensive against them

1. **Config not threaded through ProvisionRegistrar.** SC core's sidecar in
   api v1.0 only sent the type token; `cfg.AccountId` was empty on the
   plugin side, `cfImpl.NewProvider(ctx, "", ...)` errored. SC core now
   serializes the config to InputValues before the RPC. If you're
   debugging a "resource name argument cannot be empty" error against this
   plugin, you're on a stale SC core — bump to the one that includes
   `serializeConfigToInputs`.

2. **MonitorProxy missing Invoke / Call / ReadResource.** Cloudflare's
   `LookupZone` goes through `monitor.Invoke(cloudflare:index/getZone:getZone)`.
   SC core's MonitorProxy had only RegisterResource + SupportsFeature +
   RegisterResourceOutputs. If you see "method Invoke not implemented" at
   plugin init time, same stale-sc-core diagnosis.

3. **`cfImpl.GetZoneFilterAccountId` renamed to `GetZoneFilterAccount` in
   pulumi-cloudflare v6.17.** The lifted driver code already uses the new
   name; don't rename it back.

4. **`LookupZone` with `Match: "any"` matched too broadly** when a token
   has visibility into multiple accounts with same-named zones. The
   extracted driver pins `Match: "all"` + `Account.Id` so the lookup is
   deterministic by account. The in-tree pre-split driver (now dormant in
   api) still has the old shape; don't copy it back.

## When this repo lands as a merged dependency

The remaining open work on the sc-plugin-architecture track is:
- api PR that drops `pkg/clouds/{cloudflare,pulumi/cloudflare}/` + removes
  `pulumi-cloudflare` from `go.mod`, gated on every fleet service pinning
  `sc-plugin-cloudflare@<version>` in its `server.yaml` first.
- A release-automation workflow in this repo (`.github/workflows/release.yml`)
  that cuts tagged binaries + sha256sums for every platform SC supports.

Both are open roadmap items in forge.

## Commit attribution

Never add a `Co-Authored-By` trailer to a commit, and never append a session
link. Same rule as every other simple-container-com repo. DCO sign-off
(`git commit -s`) is required.
