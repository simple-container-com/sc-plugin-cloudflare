// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// URN contract for cloudflare-static-website. The driver produces two
// Pulumi resources (PagesProject + optional PagesDomain) and runs an
// in-plugin REST upload inside an ApplyT. These tests pin both the
// URN names and the shape of the REST calls against a mock CF server.

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

// fakeCFServer is a minimal httptest.Server that implements the four
// Pages Direct Upload endpoints + the deployment-creation POST. Captures
// every request so tests can assert the plugin called them in order.
type fakeCFServer struct {
	srv            *httptest.Server
	mu             sync.Mutex
	tokenRequests  int
	missingHashes  []string
	uploadedHashes []string
	upsertedHashes []string
	deploymentPOST bool
	manifest       map[string]string
}

func newFakeCFServer(t *testing.T) *fakeCFServer {
	t.Helper()
	f := &fakeCFServer{manifest: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && filepath.Base(r.URL.Path) == "upload-token":
			f.tokenRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"jwt": "fake-jwt"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/pages/assets/check-missing":
			var body struct {
				Hashes []string `json:"hashes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Pretend none exist yet — all hashes are missing.
			f.missingHashes = append(f.missingHashes, body.Hashes...)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  body.Hashes,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/pages/assets/upload":
			var payloads []uploadPayload
			_ = json.NewDecoder(r.Body).Decode(&payloads)
			for _, p := range payloads {
				f.uploadedHashes = append(f.uploadedHashes, p.Key)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": nil})
		case r.Method == http.MethodPost && r.URL.Path == "/pages/assets/upsert-hashes":
			var body struct {
				Hashes []string `json:"hashes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.upsertedHashes = append(f.upsertedHashes, body.Hashes...)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": nil})
		case r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "deployments":
			f.deploymentPOST = true
			_ = r.ParseMultipartForm(16 << 20)
			manifestField := r.PostForm.Get("manifest")
			if manifestField == "" && r.MultipartForm != nil {
				if v := r.MultipartForm.Value["manifest"]; len(v) > 0 {
					manifestField = v[0]
				}
			}
			_ = json.Unmarshal([]byte(manifestField), &f.manifest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"id": "dep-123", "url": "https://mysite.pages.dev"},
			})
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Setenv("SC_CLOUDFLARE_API_BASE", f.srv.URL)
	t.Cleanup(f.srv.Close)
	return f
}

// bundleWithOneFile creates a temp bundle directory with a single
// `index.html` so the uploader has something to hash + upload.
func bundleWithOneFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPages_URNContract_NoDomain — a stack without `domain:` registers
// only a PagesProject named `<stack>-pages`. The REST uploader is
// still called at Apply (one file, one upload, one deployment POST).
func TestPages_URNContract_NoDomain(t *testing.T) {
	RegisterTestingT(t)

	fake := newFakeCFServer(t)
	bundle := bundleWithOneFile(t)

	m := &pagesMocks{}
	reg := &capturingRegistrar{}
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(bundle)
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
	Expect(m.namesByType("cloudflare:index/pagesDomain:PagesDomain")).To(BeEmpty(),
		"PagesDomain must not be created when `domain:` is unset")
	Expect(reg.records).To(BeEmpty(),
		"no registrar record when `domain:` is unset")

	// Upload flow fired end-to-end.
	Expect(fake.tokenRequests).To(Equal(1), "upload-token GET must fire exactly once per deploy")
	Expect(fake.uploadedHashes).To(HaveLen(1), "one file in bundle → one hash uploaded")
	Expect(fake.deploymentPOST).To(BeTrue(), "deployments POST must fire after upload")
	Expect(fake.manifest).To(HaveKey("/index.html"), "manifest keys must be forward-slash-prefixed paths")
}

// TestPages_URNContract_WithDomain — a stack with `domain:` adds a
// PagesDomain named `<stack>-domain` and a Registrar CNAME at
// cfg.Site.Domain → `<stack>.pages.dev`, proxied.
func TestPages_URNContract_WithDomain(t *testing.T) {
	RegisterTestingT(t)

	newFakeCFServer(t)
	bundle := bundleWithOneFile(t)

	m := &pagesMocks{}
	reg := &capturingRegistrar{}
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		cfg := baseInput(bundle)
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
