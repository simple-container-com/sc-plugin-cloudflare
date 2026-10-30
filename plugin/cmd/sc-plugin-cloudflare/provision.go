// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	cfApi "github.com/simple-container-com/sc-plugin-cloudflare/api"
	cfInternal "github.com/simple-container-com/sc-plugin-cloudflare/plugin/internal"
)

// provisionFunc is the shape every cloudflare driver in plugin/internal/*
// exports. Same signature as sc-plugin-aws's.
type provisionFunc func(*sdk.Context, api.Stack, api.ResourceInput, pApi.ProvisionParams) (*api.ResourceOutput, error)

// provisionDispatch is the resource-type → driver map. v1 ships one entry
// for the cloudflare-static-website template; add future resources here as
// they land.
var provisionDispatch = map[string]provisionFunc{
	cfApi.TemplateTypeCloudflareStaticWebsite: cfInternal.StaticWebsite,
}

// Provision handles the per-resource provisioning RPC. SC core sends one
// of these per `resource` / `template` in a client stack that routes to a
// plugin-advertised type. Dispatch pattern mirrors sc-plugin-aws:
// decode the typed config, open a pulumi.Context against the host monitor,
// run the driver, drain the context so Pulumi's async RegisterResource
// goroutines reach the monitor before the RPC returns.
func (p *cfPlugin) Provision(ctx context.Context, req *scpluginv1.ProvisionRequest) (resp *scpluginv1.ProvisionResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			err = errors.Errorf("sc-plugin-cloudflare Provision panic: %v\n%s", r, stack)
		}
	}()
	fn, ok := provisionDispatch[req.GetResourceType()]
	if !ok {
		return nil, errors.Errorf("sc-plugin-cloudflare does not know how to provision %q", req.GetResourceType())
	}

	pctx, _, err := p.pulumiContextFromHeader(ctx, req.GetHeader())
	if err != nil {
		return nil, err
	}
	// DO NOT close pctx here. See the aws plugin's identical note at
	// sc-plugin-aws/plugin/cmd/sc-plugin-aws/main.go:222 — closing the
	// context before every ApplyT the driver scheduled has drained can
	// take background goroutines down with it. The pctx is cheap to leak
	// until plugin shutdown — one TCP connection to SC core's monitor.
	p.progress.Info("provision", fmt.Sprintf("provisioning %s %q", req.GetResourceType(), req.GetResourceName()))

	cfg, err := decodeTypedConfig(req.GetResourceType(), req.GetInputs())
	if err != nil {
		return nil, err
	}

	// `input.StackParams.StackName` must carry the PARENT CLI name — same
	// rationale as the aws plugin, same fallback for pre-cli_stack_name
	// SC core builds.
	cliStackName := req.GetHeader().GetCliStackName()
	if cliStackName == "" {
		cliStackName = req.GetHeader().GetStackName()
	}
	input := api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   req.GetResourceType(),
			Name:   req.GetResourceName(),
			Config: api.Config{Config: cfg},
		},
		StackParams: &api.StackParams{
			StackName:   cliStackName,
			Environment: req.GetHeader().GetEnvironment(),
		},
	}

	params := pApi.ProvisionParams{
		Log:              newProgressLogger(p.progress, req.GetResourceType()),
		ComputeContext:   newCapturingCollector(),
		BaseEnvVariables: map[string]string{},
	}

	// v1 invariant: cloudflare-static-website requires the cloudflare
	// registrar in the same plugin process. The Pages custom-domain
	// binding (PagesDomain) only accepts domains that live in a Cloudflare
	// zone owned by the same account, so the registrar for any
	// cloudflare-static-website stack is always this plugin. Reuse the
	// local registrar directly instead of taking an RPC detour through
	// SC core's PeerInvoke.
	//
	// If the parent server.yaml does not configure a cloudflare registrar
	// at all, p.registrar is nil and the driver fails with a clear
	// error pointing at the parent — same shape as sc-plugin-aws when
	// RegistrarPluginName is empty.
	p.mu.Lock()
	params.Registrar = p.registrar
	p.mu.Unlock()

	out, err := fn(pctx, api.Stack{Name: cliStackName}, input, params)
	if err != nil {
		p.progress.Error("provision", err.Error())
		return nil, err
	}
	if werr := drainContext(pctx); werr != nil {
		p.progress.Error("provision", "drain: "+werr.Error())
		return nil, werr
	}
	urn := waitForRef(out.Ref)
	p.progress.Info("provision", fmt.Sprintf("%s %q registered (urn=%s)", req.GetResourceType(), req.GetResourceName(), urn))
	return &scpluginv1.ProvisionResponse{Urn: urn}, nil
}

// decodeTypedConfig picks the Go struct for a resource type's config.
// Mirrors sc-plugin-aws's decoder. The cloudflare api package registers
// each type with api.RegisterProviderConfig at init() time — this helper
// is the in-plugin twin.
func decodeTypedConfig(resourceType string, inputs map[string]*scpluginv1.InputValue) (any, error) {
	switch resourceType {
	case cfApi.TemplateTypeCloudflareStaticWebsite:
		cfg := &cfApi.PagesConfig{}
		return cfg, decodeInputsAs(inputs, cfg)
	default:
		return nil, errors.Errorf("sc-plugin-cloudflare has no config decoder for %q", resourceType)
	}
}

// decodeInputsAs lifts the plugin bus's input map into a typed struct via
// yaml round-trip. Mirrors sc-plugin-aws's decoder. Scalar-only decoding
// would drop nested maps and lists; see the aws file's live-caught note.
func decodeInputsAs[T any](inputs map[string]*scpluginv1.InputValue, out *T) error {
	plain := make(map[string]any, len(inputs))
	for k, v := range inputs {
		plain[k] = unwrapInputValue(v)
	}
	b, err := yaml.Marshal(plain)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

// unwrapInputValue recursively flattens an InputValue tree back into Go
// types the yaml round-trip can unmarshal into a typed struct.
func unwrapInputValue(v *scpluginv1.InputValue) any {
	if v == nil {
		return nil
	}
	switch x := v.GetValue().(type) {
	case *scpluginv1.InputValue_StringValue:
		return x.StringValue
	case *scpluginv1.InputValue_BoolValue:
		return x.BoolValue
	case *scpluginv1.InputValue_DoubleValue:
		return x.DoubleValue
	case *scpluginv1.InputValue_BytesValue:
		return string(x.BytesValue)
	case *scpluginv1.InputValue_MapValue:
		m := make(map[string]any, len(x.MapValue.GetFields()))
		for k, iv := range x.MapValue.GetFields() {
			m[k] = unwrapInputValue(iv)
		}
		return m
	case *scpluginv1.InputValue_ListValue:
		items := x.ListValue.GetItems()
		out := make([]any, len(items))
		for i, iv := range items {
			out[i] = unwrapInputValue(iv)
		}
		return out
	}
	return nil
}

// capturingCollector is a minimal pApi.ComputeContextCollector that
// records nothing — cloudflare-static-website is a terminal template that
// does not propagate env vars or tpl extensions to a parent compute. If a
// future cloudflare resource needs collector semantics, port the full
// shape from sc-plugin-aws/plugin/cmd/sc-plugin-aws/collector.go.
type capturingCollector struct {
	mu sync.Mutex
}

func newCapturingCollector() *capturingCollector { return &capturingCollector{} }

func (c *capturingCollector) SecretEnvVariables() []pApi.ComputeEnvVariable { return nil }
func (c *capturingCollector) EnvVariables() []pApi.ComputeEnvVariable       { return nil }
func (c *capturingCollector) AddEnvVariableIfNotExist(_, _, _, _, _ string) {}
func (c *capturingCollector) AddSecretEnvVariableIfNotExist(_, _, _, _, _ string) {
}

func (c *capturingCollector) AddResourceTplExtension(_ string, _ map[string]string)      {}
func (c *capturingCollector) AddDependencyTplExtension(_, _ string, _ map[string]string) {}
func (c *capturingCollector) AddDependency(sdk.Resource)                                 {}
func (c *capturingCollector) Dependencies() []sdk.Resource                               { return nil }
func (c *capturingCollector) AddOutput(_ *sdk.Context, _ sdk.Output)                     {}
func (c *capturingCollector) Outputs() []sdk.Output                                      { return nil }

func (c *capturingCollector) ResolvePlaceholders(any) error                    { return nil }
func (c *capturingCollector) AddPreProcessor(any, pApi.PreProcessor)           {}
func (c *capturingCollector) AddPostProcessor(any, pApi.PostProcessor)         {}
func (c *capturingCollector) GetPreProcessors(any) ([]pApi.PreProcessor, bool) { return nil, false }

func (c *capturingCollector) GetPostProcessors(any) ([]pApi.PostProcessor, bool) { return nil, false }

func (c *capturingCollector) RunPreProcessors(any, any) error { return nil }

func (c *capturingCollector) RunPostProcessors(any, any) error { return nil }

func (c *capturingCollector) ResourceOutputs() pApi.ResourcesOutputs { return nil }
