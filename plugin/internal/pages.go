// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/pkg/errors"
	cfImpl "github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"

	cfApi "github.com/simple-container-com/sc-plugin-cloudflare/api"
)

// StaticWebsite is the driver for `template: cloudflare-static-website`.
//
// Three resources + one in-plugin REST upload, in order:
//
//  1. cfImpl.NewPagesProject — Direct Upload mode (nil Source). pulumi-cloudflare
//     v6.17.0 has no NewPagesDeployment resource; we upload the bundle below.
//  2. project.Name.ApplyT(...) — calls `uploader.uploadBundle` which does
//     wrangler's five-call flow (upload-token → check-missing → upload →
//     upsert-hashes → create-deployment) directly in Go. No wrangler, no
//     node, no local shell-out. See uploader.go for the API reference.
//  3. cfImpl.NewPagesDomain — custom-domain binding (only if cfg.Site.Domain
//     is set).
//  4. params.Registrar.NewRecord — CNAME `<domain>` → `<project>.pages.dev`,
//     proxied through Cloudflare (only if cfg.Site.Domain is set).
//
// The upload is NOT a Pulumi resource — ApplyT runs during Apply (not
// Preview), so a `pulumi preview` lists only the three real resources
// and skips the network call. On Apply, Cloudflare retains every
// deployment and the custom-domain binding always points at the latest
// production deployment, so repeated applies with unchanged content are
// cheap (check-missing short-circuits) and idempotent on the user-visible
// side even without Pulumi state tracking the upload.
func StaticWebsite(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	if input.Descriptor.Type != cfApi.TemplateTypeCloudflareStaticWebsite {
		return nil, errors.Errorf("unsupported template type %q", input.Descriptor.Type)
	}
	cfg, ok := input.Descriptor.Config.Config.(*cfApi.PagesInput)
	if !ok {
		return nil, errors.Errorf("failed to convert config to *cloudflare.PagesInput for %q (got %T)",
			input.Descriptor.Type, input.Descriptor.Config.Config)
	}
	if cfg.AccountId == "" {
		return nil, errors.Errorf("accountId is empty for cloudflare-static-website %q", stack.Name)
	}
	if cfg.BundleDir == "" {
		return nil, errors.Errorf("bundleDir is empty for cloudflare-static-website %q", stack.Name)
	}
	bundleDir, err := filepath.Abs(cfg.BundleDir)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve bundleDir %q to absolute path", cfg.BundleDir)
	}
	productionBranch := cfg.ProductionBranch
	if productionBranch == "" {
		productionBranch = "main"
	}

	// Build a cloudflare provider from the stack's own API token BEFORE
	// creating any Pages resource. Without an explicit `sdk.Provider(...)`,
	// `cfImpl.NewPagesProject` falls through to Pulumi's default
	// `pulumi:providers:cloudflare::default_6_17_0` which has no credentials
	// configured, and the Cloudflare API answers:
	//   400 Bad Request: {"code":9106,"message":"Missing X-Auth-Key,
	//   X-Auth-Email or Authorization headers"}
	// Live-caught on navizon-landing deploy run 41244391104 (2026-10-31)
	// — the apply reached PagesProject create cleanly, hit this, errored.
	// Mirrors the registrar's own provider construction in registrar.go.
	params.Log.Info(ctx.Context(), "configure cloudflare provider for stack %q (account %q)...", stack.Name, cfg.AccountId)
	provider, err := cfImpl.NewProvider(ctx, fmt.Sprintf("%s-pages-provider", stack.Name), &cfImpl.ProviderArgs{
		ApiToken: sdk.StringPtr(cfg.Credentials.Credentials),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to init cloudflare provider for pages %q", stack.Name)
	}

	// Build the project. Nil Source → direct-upload mode; Pulumi creates
	// the Pages project itself, and the uploader below publishes against it.
	projectName := stack.Name
	params.Log.Info(ctx.Context(), "configure cloudflare pages project %q...", projectName)
	project, err := cfImpl.NewPagesProject(ctx, fmt.Sprintf("%s-pages", stack.Name), &cfImpl.PagesProjectArgs{
		AccountId:        sdk.String(cfg.AccountId),
		Name:             sdk.String(projectName),
		ProductionBranch: sdk.String(productionBranch),
	}, sdk.Provider(provider))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create cloudflare pages project %q", projectName)
	}

	// Deploy the bundle inside an ApplyT so (a) the project name is
	// resolved from the Create's response and (b) the network call only
	// fires at Apply, not Preview. We attach the upload result as an
	// Output so it participates in the resource graph for logging.
	//
	// `ctx.DryRun()` guard: Pulumi SDK will fire this ApplyT during
	// preview if `project.Name` resolves to a known value (which it does
	// here, since the input is a plain string literal). During preview
	// the PagesProject itself hasn't been created on Cloudflare, so an
	// upload-token GET against a nonexistent project either hangs on
	// the net stack (observed: navizon run 41195707959, 2026-10-31,
	// 20-min hang before manual cancel) or 404s. Skip the upload during
	// preview and only fire at apply.
	deployment := project.Name.ApplyT(func(name string) (string, error) {
		if ctx.DryRun() {
			return "preview-no-deploy", nil
		}
		uploader := newPagesUploader(cfg.Credentials.Credentials, cfg.AccountId)
		res, uerr := uploader.uploadBundle(
			ctx.Context(),
			name,
			bundleDir,
			productionBranch,
			"", // commit message — SC doesn't thread one through yet
			"", // commit hash — same
			&pagesUploadLogger{inner: params.Log},
		)
		if uerr != nil {
			return "", uerr
		}
		return res.ID, nil
	}).(sdk.StringOutput)

	// No domain → serve on <project>.pages.dev only. Still gate on the
	// deployment Output so a preview chain of this with downstream
	// consumers works.
	if cfg.Site.Domain == "" {
		params.Log.Info(ctx.Context(),
			"cloudflare pages project %q has no `domain:` set; site will serve at %s.pages.dev",
			projectName, projectName)
		return &api.ResourceOutput{Ref: deployment}, nil
	}

	// Custom-domain binding + Registrar CNAME.
	params.Log.Info(ctx.Context(), "configure cloudflare pages custom domain %q...", cfg.Site.Domain)
	_, err = cfImpl.NewPagesDomain(ctx, fmt.Sprintf("%s-domain", stack.Name), &cfImpl.PagesDomainArgs{
		AccountId:   sdk.String(cfg.AccountId),
		ProjectName: project.Name,
		Name:        sdk.String(cfg.Site.Domain),
	}, sdk.Provider(provider), sdk.DependsOn([]sdk.Resource{project}))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to attach cloudflare pages domain %q to project %q", cfg.Site.Domain, projectName)
	}

	if params.Registrar == nil {
		return nil, errors.Errorf(
			"cloudflare-static-website %q sets `domain: %s` but no registrar is configured on this stack — "+
				"add a `resources.registrars.cloudflare` entry on the parent so SC can create the CNAME",
			stack.Name, cfg.Site.Domain,
		)
	}
	cnameTarget := fmt.Sprintf("%s.pages.dev", projectName)
	_, err = params.Registrar.NewRecord(ctx, api.DnsRecord{
		Name:     cfg.Site.Domain,
		Type:     "CNAME",
		ValueOut: sdk.String(cnameTarget).ToStringOutput(),
		Proxied:  true,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to provision CNAME %q → %q", cfg.Site.Domain, cnameTarget)
	}

	return &api.ResourceOutput{Ref: deployment}, nil
}

// pagesUploadLogger adapts pApi.Logger to the uploader's slim interface.
// Keeps the uploader free of the SC-core logger import for easier testing.
type pagesUploadLogger struct {
	inner logger.Logger
}

func (p *pagesUploadLogger) Info(ctx context.Context, msg string, args ...any) {
	p.inner.Info(ctx, msg, args...)
}
