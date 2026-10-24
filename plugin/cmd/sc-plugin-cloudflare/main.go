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
	"github.com/simple-container-com/api/pkg/api/logger"
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
}

func (p *cfPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:  "cloudflare-0.0.1",
		RegistrarTypes: []string{"cloudflare"},
		PulumiPlugins: []*scpluginv1.PulumiPluginRequirement{
			{Package: "cloudflare", Version: "6.17.0"},
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
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "open pulumi context from monitor address")
	}
	return pctx, func() { pctx.Close() }, nil
}

func (p *cfPlugin) ProvisionRegistrar(ctx context.Context, req *scpluginv1.ProvisionRegistrarRequest) (*scpluginv1.ProvisionRegistrarResponse, error) {
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
	r, err := cfInternal.Registrar(pctx, desc, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		return nil, errors.Wrap(err, "init cloudflare registrar")
	}
	p.mu.Lock()
	p.registrar = r
	p.config = cfg
	p.mu.Unlock()
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
	out, err := r.NewRecord(pctx, api.DnsRecord{
		Name:    req.GetRecordName(),
		Type:    req.GetRecordType(),
		Value:   req.GetValueResolved(),
		Proxied: req.GetProxied(),
	})
	if err != nil {
		return nil, err
	}
	return &scpluginv1.NewDnsRecordResponse{Urn: refToString(out.Ref)}, nil
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
	out, err := r.NewOverrideHeaderRule(pctx, api.Stack{Name: req.GetStackName()}, rule)
	if err != nil {
		return nil, err
	}
	return &scpluginv1.NewOverrideHeaderRuleResponse{Urn: refToString(out.Ref)}, nil
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
	out, err := r.ProvisionDomainForEndpoint(pctx, api.Stack{Name: req.GetStackName()}, endpoint)
	if err != nil {
		return nil, err
	}
	return &scpluginv1.ProvisionDomainForEndpointResponse{Urn: refToString(out.Ref)}, nil
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
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: &cfPlugin{},
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-plugin-cloudflare",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}
