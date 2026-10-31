// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-plugin-cloudflare is the first out-of-tree SC cloud driver, extracted
// from pkg/clouds/{cloudflare,pulumi/cloudflare} per the Phase 2 extraction
// checklist in forge/docs/design/2026-10-24/sc-plugin-architecture/.
//
// The plugin binary advertises registrar_types=["cloudflare"] and
// implements the registrar RPCs on the SC plugin bus. Each RPC constructs
// a short-lived pulumi.Context against the host's monitor address and
// reuses the lifted internal/*.go driver code unchanged — the extraction
// is pure code movement, no API changes.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	cfApi "github.com/simple-container-com/sc-plugin-cloudflare/api"
	cfInternal "github.com/simple-container-com/sc-plugin-cloudflare/plugin/internal"
)

type cfPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer

	mu        sync.Mutex
	registrar pApi.Registrar
	config    *cfApi.RegistrarConfig
	// progress is the plugin's side of the LogStream RPC. Non-nil at
	// Serve() time; the gRPC server drains it via LogStream. The lifted
	// registrar driver pushes to this too, through the progressLogger
	// handed to it as `pApi.ProvisionParams.Log` at ProvisionRegistrar.
	//
	// This comment described that adaptor before it existed — the driver
	// was in fact given `logger.New()` and logged into the subprocess's
	// stdout, where nothing read it. See progresslog.go.
	progress *scPlugin.Publisher
}

func (p *cfPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:  "cloudflare-0.0.1",
		RegistrarTypes: []string{"cloudflare"},
		ResourceTypes:  []string{cfApi.TemplateTypeCloudflareStaticWebsite},
		PulumiPlugins: []*scpluginv1.PulumiPluginRequirement{
			{Package: "cloudflare", Version: "6.17.0"},
			// No pulumi-command: cloudflare-static-website uploads the
			// bundle via an in-plugin REST client (uploader.go) rather
			// than shelling out to wrangler. The deploy image needs no
			// node/npm/wrangler as a result — see
			// forge/docs/design/2026-10-31/cloudflare-pages-direct-upload/
			// and workflow #41181349116's wrangler-not-in-image constraint.
		},
	}, nil
}

func (p *cfPlugin) pulumiContextFromHeader(ctx context.Context, h *scpluginv1.RequestHeader) (*sdk.Context, func(), error) {
	if h.GetMonitorAddress() == "" {
		return nil, nil, errors.New("no monitor address in request header")
	}
	pctx, err := sdk.NewContext(ctx, sdk.RunInfo{
		MonitorAddr: h.GetMonitorAddress(),
		Project:     h.GetProjectName(),
		Stack:       h.GetStackName(),
		// DryRun landed on RequestHeader via api v2026.10.60 (api#527).
		// Without this the plugin's pctx thinks DryRun() is always false,
		// which broke the Pages uploader's `if ctx.DryRun()` guard at
		// plugin/internal/pages.go:94 — uploader fired at preview against
		// a PagesProject that only existed in the plan, hung 30 min on
		// upload-token until the GH Actions runner killed the job
		// (navizon-landing run 41225424219, 2026-10-31). Threading
		// h.GetDryRun() here makes the guard actually work. Zero-value
		// false on older SC cores matches pre-field behaviour so this
		// change is transparent against pre-10.60 cores.
		DryRun: h.GetDryRun(),
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "open pulumi context from monitor address")
	}
	return pctx, func() { pctx.Close() }, nil
}

func (p *cfPlugin) ProvisionRegistrar(ctx context.Context, req *scpluginv1.ProvisionRegistrarRequest) (*scpluginv1.ProvisionRegistrarResponse, error) {
	// Say which zone is the CONFIGURED DEFAULT and which the stack asked for,
	// because this line is emitted before the zone is resolved and in
	// isolation it reads like the final answer. On the fleet the configured
	// default is the catch-all `simple-container.com` while every stack
	// overrides it, so a bare `for zone "simple-container.com"` here looks
	// exactly like the wrong-zone defect this plugin was released to fix —
	// it cost a reviewer time on run 40654637631, a run that had in fact
	// resolved the override correctly. The `ready; zone …` line below is the
	// authoritative one.
	initZone := fmt.Sprintf("%q (configured default)", strVal(req.GetConfig()["zoneName"]))
	if baseZone := req.GetBaseDnsZone(); baseZone != "" {
		initZone += fmt.Sprintf(", stack prefers %q", baseZone)
	} else {
		initZone += ", stack states no preference"
	}
	p.progress.Info("provision-registrar", "initialising cloudflare registrar; zone "+initZone)
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()

	cfg := inputsToRegistrarConfig(req.GetConfig())
	desc := api.RegistrarDescriptor{
		Type:   "cloudflare",
		Config: api.Config{Config: cfg},
	}
	// The deploying stack's preferred base zone. cfInternal.Registrar already
	// honours this (it looks up THAT zone instead of cfg.ZoneName), but the
	// field did not exist on the plugin bus until api #497, so the override
	// could never fire and the registrar always resolved its own configured
	// zone. For the fleet, whose parent server.yaml declares one catch-all
	// account-wide registrar and publishes every service under a different
	// zone, that meant writing the record into the WRONG zone — a changed
	// zone_id is an input diff, so Pulumi replaced the record
	// create-before-delete and the delete half removed the LIVE one, while
	// reporting a green deploy. Empty (older core, or a stack with no
	// baseDnsZone) keeps the previous behaviour.
	var pref *pApi.DnsPreference
	if baseZone := req.GetBaseDnsZone(); baseZone != "" {
		pref = &pApi.DnsPreference{BaseZone: baseZone}
	}
	// Log through the progress Publisher, NOT logger.New(). The driver's own
	// Info lines ("configure CNAME DNS record …", the worker-script lines)
	// went to the plugin subprocess's stdout and never reached the operator —
	// zero of them appear in forge-notifier run 40654637631. See
	// progresslog.go for why, and for the phase-coverage trap it closes.
	r, err := cfInternal.Registrar(pctx, desc, pApi.ProvisionParams{
		Log:           newProgressLogger(p.progress, "registrar"),
		DnsPreference: pref,
	})
	if err != nil {
		p.progress.Error("provision-registrar", "init failed: "+err.Error())
		return nil, errors.Wrap(err, "init cloudflare registrar")
	}
	p.mu.Lock()
	p.registrar = r
	p.config = cfg
	p.mu.Unlock()
	// Name the zone that was actually resolved, not just the main domain. The
	// wrong-zone defect above was invisible in the deploy log precisely
	// because nothing on the progress channel ever said which zone won.
	effectiveZone := cfg.ZoneName
	if pref != nil {
		effectiveZone = pref.BaseZone + " (stack override of " + cfg.ZoneName + ")"
	}
	p.progress.Info("provision-registrar", fmt.Sprintf("ready; zone %s, main domain is %q", effectiveZone, r.MainDomain()))
	return &scpluginv1.ProvisionRegistrarResponse{MainDomain: r.MainDomain()}, nil
}

func (p *cfPlugin) NewDnsRecord(ctx context.Context, req *scpluginv1.NewDnsRecordRequest) (*scpluginv1.NewDnsRecordResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("NewDnsRecord called before ProvisionRegistrar")
	}
	p.progress.Info("dns-record", fmt.Sprintf("creating %s record %q → %q", req.GetRecordType(), req.GetRecordName(), req.GetValueResolved()))
	out, err := r.NewRecord(pctx, api.DnsRecord{
		Name:    req.GetRecordName(),
		Type:    req.GetRecordType(),
		Value:   req.GetValueResolved(),
		Proxied: req.GetProxied(),
	})
	if err != nil {
		p.progress.Error("dns-record", fmt.Sprintf("record %q failed: %v", req.GetRecordName(), err))
		return nil, err
	}
	// Block until Pulumi's async RegisterResource has fired. See waitForRef.
	urn := waitForRef(out.Ref)
	p.progress.Info("dns-record", fmt.Sprintf("record %q registered (urn=%s)", req.GetRecordName(), urn))
	return &scpluginv1.NewDnsRecordResponse{Urn: urn}, nil
}

// waitForRef blocks until a Pulumi Output in a resource ref resolves. This
// is what makes the per-RPC pulumi.Context model safe: Pulumi's SDK fires
// RegisterResource inside an async goroutine, so returning from an RPC
// handler before the output resolves and then letting `defer pctx.Close()`
// run would tear down the engine connection mid-registration — the request
// vanishes silently and no resource lands in state. Waiting on the output
// ensures the registration RPC has at least been sent (and in practice has
// completed and resolved the ID) before Close() fires.
func waitForRef(ref any) string {
	if ref == nil {
		return ""
	}
	switch r := ref.(type) {
	case string:
		return r
	case sdk.IDOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(id sdk.ID) sdk.ID {
			select {
			case ch <- string(id):
			default:
			}
			return id
		})
		return <-ch
	case sdk.URNOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(u sdk.URN) sdk.URN {
			select {
			case ch <- string(u):
			default:
			}
			return u
		})
		return <-ch
	case sdk.StringOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(s string) string {
			select {
			case ch <- s:
			default:
			}
			return s
		})
		return <-ch
	default:
		return fmt.Sprintf("%v", r)
	}
}

func (p *cfPlugin) NewOverrideHeaderRule(ctx context.Context, req *scpluginv1.NewOverrideHeaderRuleRequest) (*scpluginv1.NewOverrideHeaderRuleResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("NewOverrideHeaderRule called before ProvisionRegistrar")
	}
	rule := pApi.OverrideHeaderRule{
		Name:       req.GetRuleName(),
		FromHost:   req.GetFromHost(),
		ToHost:     sdk.String(req.GetToHostResolved()),
		PathPrefix: req.GetPathPrefix(),
	}
	if ba := req.GetBasicAuth(); ba != nil {
		rule.BasicAuth = &pApi.BasicAuth{Username: ba.GetUsername(), Password: ba.GetPassword(), Realm: ba.GetRealm()}
	}
	if op := req.GetOverridePages(); op != nil {
		rule.OverridePages = &pApi.OverridePagesRule{IndexPage: op.GetIndexPage(), NotFoundPage: op.GetNotFoundPage()}
	}
	p.progress.Info("worker-route", fmt.Sprintf("overriding host %q → %q (rule %q)", rule.FromHost, req.GetToHostResolved(), rule.Name))
	out, err := r.NewOverrideHeaderRule(pctx, api.Stack{Name: req.GetStackName()}, rule)
	if err != nil {
		p.progress.Error("worker-route", fmt.Sprintf("rule %q failed: %v", rule.Name, err))
		return nil, err
	}
	urn := waitForRef(out.Ref)
	p.progress.Info("worker-route", fmt.Sprintf("rule %q registered (urn=%s)", rule.Name, urn))
	return &scpluginv1.NewOverrideHeaderRuleResponse{Urn: urn}, nil
}

func (p *cfPlugin) ProvisionDomainForEndpoint(ctx context.Context, req *scpluginv1.ProvisionDomainForEndpointRequest) (*scpluginv1.ProvisionDomainForEndpointResponse, error) {
	pctx, done, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	defer done()
	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("ProvisionDomainForEndpoint called before ProvisionRegistrar")
	}
	endpoint := pApi.DomainEndpoint{
		Name:       req.GetEndpointName(),
		Domain:     req.GetDomain(),
		TargetHost: sdk.String(req.GetTargetHostResolved()),
		WebSocket:  req.GetWebsocket(),
	}
	p.progress.Info("endpoint", fmt.Sprintf("provisioning domain %q for endpoint %q (target %q)", endpoint.Domain, endpoint.Name, req.GetTargetHostResolved()))
	out, err := r.ProvisionDomainForEndpoint(pctx, api.Stack{Name: req.GetStackName()}, endpoint)
	if err != nil {
		p.progress.Error("endpoint", fmt.Sprintf("endpoint %q failed: %v", endpoint.Name, err))
		return nil, err
	}
	urn := waitForRef(out.Ref)
	p.progress.Info("endpoint", fmt.Sprintf("endpoint %q bound (urn=%s)", endpoint.Name, urn))
	return &scpluginv1.ProvisionDomainForEndpointResponse{Urn: urn}, nil
}

func inputsToRegistrarConfig(m map[string]*scpluginv1.InputValue) *cfApi.RegistrarConfig {
	return &cfApi.RegistrarConfig{
		AuthConfig: cfApi.AuthConfig{
			Credentials: api.Credentials{Credentials: strVal(m["credentials"])},
			AccountId:   strVal(m["accountId"]),
		},
		ZoneName: strVal(m["zoneName"]),
	}
}

func strVal(v *scpluginv1.InputValue) string {
	if v == nil {
		return ""
	}
	return v.GetStringValue()
}

func refToString(ref any) string {
	switch r := ref.(type) {
	case string:
		return r
	case sdk.URNOutput:
		return "output:urn"
	case sdk.IDOutput:
		return "output:id"
	default:
		return fmt.Sprintf("%v", r)
	}
}

func main() {
	// Full goroutine traces on panic. Same reason sc-plugin-aws +
	// sc-plugin-mongodb both set this: Go's default `single` traceback
	// often omits the goroutine where a nil-ptr fires.
	_ = os.Setenv("GOTRACEBACK", "all")
	impl := &cfPlugin{
		progress: scPlugin.NewPublisher(0),
	}
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: impl,
		// Debug level forwards subprocess stderr (which carries our
		// uploader progress fprintfs + any panic header/trace) through
		// SC core's hclog pipeline. Keeping it at Warn (the previous
		// value) drops those diagnostics — observed empirically on
		// navizon run 41195707959 (2026-10-31) where the uploader
		// hung on upload-token and we had no in-flight log line to
		// confirm whether the HTTP request even completed. api PR #522
		// dropped the SC-core-side filter to Debug; this change does
		// the same on the plugin side so diagnostics make it out.
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-plugin-cloudflare",
			Level:  hclog.Debug,
			Output: os.Stderr,
		}),
	})
}

// LogStream drains the plugin's progress Publisher onto the gRPC
// stream. SC core opens one of these per plugin after Schema() returns.
// Ends when the stream context cancels (SC core teardown) or Serve()
// stops accepting (plugin exit).
func (p *cfPlugin) LogStream(_ *scpluginv1.LogStreamRequest, stream scpluginv1.SCCloudPlugin_LogStreamServer) error {
	ctx := stream.Context()
	for {
		batch, ok := p.progress.Drain()
		if !ok {
			return nil
		}
		for _, ev := range batch {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}
