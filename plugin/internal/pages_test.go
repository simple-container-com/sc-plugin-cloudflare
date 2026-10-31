// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"sort"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"

	cfApi "github.com/simple-container-com/sc-plugin-cloudflare/api"
)

// URN contract for cloudflare-static-website. The driver produces four
// resources whose names derive from stack.Name; a future rename would
// replace+destroy every live Pages project on upgrade.

type pagesMocks struct {
	mu       sync.Mutex
	captured []sdk.MockResourceArgs
}

func (m *pagesMocks) NewResource(args sdk.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.captured = append(m.captured, args)
	return args.Name + "-mock-id", args.Inputs, nil
}

func (m *pagesMocks) Call(args sdk.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

func (m *pagesMocks) namesByType(typeToken string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, a := range m.captured {
		if a.TypeToken == typeToken {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

// capturingRegistrar is a stand-in for params.Registrar that records every
// NewRecord call. The driver only calls NewRecord; the other interface
// methods are not exercised by cloudflare-static-website and panic if hit.
type capturingRegistrar struct {
	records []api.DnsRecord
}

func (r *capturingRegistrar) MainDomain() string { return "example.com" }

func (r *capturingRegistrar) NewRecord(_ *sdk.Context, record api.DnsRecord) (*api.ResourceOutput, error) {
	r.records = append(r.records, record)
	return &api.ResourceOutput{Ref: "stub-record-ref"}, nil
}

func (r *capturingRegistrar) ProvisionRecords(_ *sdk.Context, _ pApi.ProvisionParams) (*api.ResourceOutput, error) {
	panic("ProvisionRecords is not called by cloudflare-static-website — test wiring is broken")
}

func (r *capturingRegistrar) NewOverrideHeaderRule(_ *sdk.Context, _ api.Stack, _ pApi.OverrideHeaderRule) (*api.ResourceOutput, error) {
	panic("NewOverrideHeaderRule is not called by cloudflare-static-website")
}

func (r *capturingRegistrar) ProvisionDomainForEndpoint(_ *sdk.Context, _ api.Stack, _ pApi.DomainEndpoint) (*api.ResourceOutput, error) {
	panic("ProvisionDomainForEndpoint is not called by cloudflare-static-website")
}

func newPagesInput(stackName string, in *cfApi.PagesInput) (api.Stack, api.ResourceInput) {
	return api.Stack{Name: stackName}, api.ResourceInput{
		Descriptor: &api.ResourceDescriptor{
			Type:   cfApi.TemplateTypeCloudflareStaticWebsite,
			Name:   stackName,
			Config: api.Config{Config: in},
		},
		StackParams: &api.StackParams{StackName: stackName, Environment: "staging"},
	}
}

// baseInput returns a minimally-valid PagesInput — pass to builders that
// then overlay fields for a specific test case.
func baseInput(bundleDir string) *cfApi.PagesInput {
	return &cfApi.PagesInput{
		TemplateConfig: cfApi.TemplateConfig{
			AuthConfig: cfApi.AuthConfig{
				Credentials: api.Credentials{Credentials: "test-token"},
				AccountId:   "acct-abc",
			},
		},
		StackConfigStatic: api.StackConfigStatic{
			BundleDir: bundleDir,
		},
	}
}

// TestPages_URNContract_NoDomain — a stack without `domain:` produces
// exactly a PagesProject and a local Command, no PagesDomain and no DNS
// record. Project name is `<stack>-pages`; deploy command name is
// `<stack>-deploy`.
func TestPages_URNContract_NoDomain(t *testing.T) {
	RegisterTestingT(t)

	tmp := t.TempDir()

	m := &pagesMocks{}
	reg := &capturingRegistrar{}
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(tmp)
		cfg.ProductionBranch = "main"
		stack, input := newPagesInput("mysite", cfg)
		_, err := StaticWebsite(ctx, stack, input, pApi.ProvisionParams{
			Log:       logger.New(),
			Registrar: reg,
		})
		return err
	}, sdk.WithMocks("test-project", "test-stack", m))
	Expect(err).ToNot(HaveOccurred())

	Expect(m.namesByType("cloudflare:index/pagesProject:PagesProject")).To(Equal([]string{"mysite-pages"}),
		"PagesProject name is `<stack>-pages`. Changing invalidates existing state.")
	Expect(m.namesByType("command:local:Command")).To(Equal([]string{"mysite-deploy"}),
		"wrangler deploy command is `<stack>-deploy`. Changing invalidates existing state.")
	Expect(m.namesByType("cloudflare:index/pagesDomain:PagesDomain")).To(BeEmpty(),
		"PagesDomain must not be created when `domain:` is unset")
	Expect(reg.records).To(BeEmpty(),
		"no registrar record when `domain:` is unset")
}

// TestPages_URNContract_WithDomain — a stack with `domain:` adds a
// PagesDomain named `<stack>-domain` and a Registrar CNAME at
// cfg.Site.Domain → `<stack>.pages.dev`, proxied.
func TestPages_URNContract_WithDomain(t *testing.T) {
	RegisterTestingT(t)

	tmp := t.TempDir()

	m := &pagesMocks{}
	reg := &capturingRegistrar{}
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(tmp)
		cfg.Site.Domain = "www.example.com"
		stack, input := newPagesInput("mysite", cfg)
		_, err := StaticWebsite(ctx, stack, input, pApi.ProvisionParams{
			Log:       logger.New(),
			Registrar: reg,
		})
		return err
	}, sdk.WithMocks("test-project", "test-stack", m))
	Expect(err).ToNot(HaveOccurred())

	Expect(m.namesByType("cloudflare:index/pagesProject:PagesProject")).To(Equal([]string{"mysite-pages"}))
	Expect(m.namesByType("command:local:Command")).To(Equal([]string{"mysite-deploy"}))
	Expect(m.namesByType("cloudflare:index/pagesDomain:PagesDomain")).To(Equal([]string{"mysite-domain"}),
		"PagesDomain name is `<stack>-domain`. Changing invalidates existing state.")

	Expect(reg.records).To(HaveLen(1))
	rec := reg.records[0]
	Expect(rec.Name).To(Equal("www.example.com"))
	Expect(rec.Type).To(Equal("CNAME"))
	Expect(rec.Proxied).To(BeTrue(),
		"the Pages CNAME must be proxied — Cloudflare serves Pages traffic through the orange cloud; a grey CNAME to `*.pages.dev` resolves but SNI breaks.")

	// Resolve the output and assert the CNAME target shape.
	ch := make(chan string, 1)
	rec.ValueOut.ApplyT(func(v string) string {
		ch <- v
		return v
	})
	Expect(<-ch).To(Equal("mysite.pages.dev"),
		"CNAME target is `<stack>.pages.dev`. The pages.dev subdomain IS the stack-level default URL Cloudflare assigns the project.")
}

// TestPages_OverriddenBaseZone — the input must honour the stack-side
// baseDnsZone so the registrar picks the right Cloudflare zone (not the
// parent registrar's default). Mirrors sc-plugin-aws's shape.
func TestPages_OverriddenBaseZone(t *testing.T) {
	RegisterTestingT(t)

	cfg := baseInput("/tmp/x")
	cfg.Site.Domain = "foo.atriumdev.app"
	cfg.Site.BaseDnsZone = "atriumdev.app"

	Expect(cfg.OverriddenBaseZone()).To(Equal("atriumdev.app"),
		"OverriddenBaseZone must return cfg.Site.BaseDnsZone — otherwise the registrar writes the CNAME into the wrong zone.")
}

// TestPages_RejectsEmptyBundleDir — a config with no BundleDir fails early
// with a config error, not a cryptic ScanDirectory error.
func TestPages_RejectsEmptyBundleDir(t *testing.T) {
	RegisterTestingT(t)

	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput("")
		stack, input := newPagesInput("mysite", cfg)
		_, err := StaticWebsite(ctx, stack, input, pApi.ProvisionParams{Log: logger.New()})
		return err
	}, sdk.WithMocks("test-project", "test-stack", &pagesMocks{}))
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("bundleDir is empty"))
}

// TestPages_RejectsEmptyAccountId — same early-fail for a config missing
// AccountId, so an operator typo doesn't reach the live Cloudflare API.
func TestPages_RejectsEmptyAccountId(t *testing.T) {
	RegisterTestingT(t)

	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(t.TempDir())
		cfg.AccountId = ""
		stack, input := newPagesInput("mysite", cfg)
		_, err := StaticWebsite(ctx, stack, input, pApi.ProvisionParams{Log: logger.New()})
		return err
	}, sdk.WithMocks("test-project", "test-stack", &pagesMocks{}))
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("accountId is empty"))
}

// TestPages_RejectsDomainWithoutRegistrar — a config that asks for a
// custom domain but has no params.Registrar must fail with the error that
// names the parent server.yaml fix.
func TestPages_RejectsDomainWithoutRegistrar(t *testing.T) {
	RegisterTestingT(t)

	tmp := t.TempDir()
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(tmp)
		cfg.Site.Domain = "www.example.com"
		stack, input := newPagesInput("mysite", cfg)
		_, err := StaticWebsite(ctx, stack, input, pApi.ProvisionParams{Log: logger.New()})
		return err
	}, sdk.WithMocks("test-project", "test-stack", &pagesMocks{}))
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("no registrar is configured"))
}
