// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/MShekow/directory-checksum/directory_checksum"
	"github.com/pkg/errors"
	cfImpl "github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	"github.com/spf13/afero"

	cfApi "github.com/simple-container-com/sc-plugin-cloudflare/api"
)

// StaticWebsite is the driver for `template: cloudflare-static-website`.
//
// Four resources, in order:
//
//  1. cfImpl.NewPagesProject — a project with nil Source for Direct Upload mode
//     (the pulumi-cloudflare SDK has no NewPagesDeployment resource, so the
//     deployment itself happens via wrangler in #2)
//  2. local.NewCommand running `wrangler pages deploy <bundleDir>` with the
//     Cloudflare token + account id passed via env. Triggers on the md5 of the
//     bundleDir's recursive content checksum, so a deploy with no file changes
//     is a Pulumi no-op
//  3. cfImpl.NewPagesDomain — the custom-domain binding (only if cfg.Domain
//     is set)
//  4. params.Registrar.NewRecord — CNAME `<domain>` → `<stackName>.pages.dev`,
//     proxied through Cloudflare (only if cfg.Domain is set)
//
// Mirrors sc-plugin-aws/plugin/internal/static_website.go on purpose — the two
// drivers are isomorphic (bucket+policy ↔ project; sync-command ↔ wrangler
// deploy; registrar CNAME ↔ registrar CNAME). See
// forge/docs/design/2026-10-31/cloudflare-pages-direct-upload/README.md for
// the full design.
func StaticWebsite(ctx *sdk.Context, stack api.Stack, input api.ResourceInput, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	if input.Descriptor.Type != cfApi.TemplateTypeCloudflareStaticWebsite {
		return nil, errors.Errorf("unsupported template type %q", input.Descriptor.Type)
	}
	cfg, ok := input.Descriptor.Config.Config.(*cfApi.PagesConfig)
	if !ok {
		return nil, errors.Errorf("failed to convert config to *cloudflare.PagesConfig for %q (got %T)",
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

	// Build the project. Nil Source → direct-upload mode; Pulumi creates the
	// Pages project itself, and wrangler publishes deployments against it.
	projectName := stack.Name
	params.Log.Info(ctx.Context(), "configure cloudflare pages project %q...", projectName)
	project, err := cfImpl.NewPagesProject(ctx, fmt.Sprintf("%s-pages", stack.Name), &cfImpl.PagesProjectArgs{
		AccountId:        sdk.String(cfg.AccountId),
		Name:             sdk.String(projectName),
		ProductionBranch: sdk.String(productionBranch),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create cloudflare pages project %q", projectName)
	}

	// Checksum the bundle so Triggers fire only on real content changes.
	// Mirrors aws-static-website's shape, minus the timestamp-override hack
	// that file notes as temporary — Pages v1 goes with the real checksum so
	// a repeat deploy of unchanged files is a Pulumi no-op.
	dir, err := directory_checksum.ScanDirectory(bundleDir, afero.NewOsFs())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to scan bundleDir %q", bundleDir)
	}
	checksums, err := dir.ComputeDirectoryChecksums()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to compute checksum of bundleDir %q", bundleDir)
	}
	sum := md5.Sum([]byte(checksums))
	bundleChecksum := hex.EncodeToString(sum[:])
	params.Log.Info(ctx.Context(), "cloudflare pages bundleDir %q checksum: %s", bundleDir, bundleChecksum)

	// wrangler is the uploader. Options evaluated in the design doc §Decision:
	// direct-REST would remove the operator-machine dependency on wrangler
	// but is significantly more code for no concrete v1 need. The Pulumi SDK
	// does not expose NewPagesDeployment, so the deployment has to happen
	// outside the resource graph either way.
	//
	// Credentials passed via env, not interpolated into the command string,
	// so they do not land in Pulumi state or in `pulumi preview --json`.
	deployCmd := sdk.Sprintf(
		"wrangler pages deploy %s --project-name=%s --branch=%s --commit-dirty=true",
		bundleDir, projectName, productionBranch,
	)
	_, err = local.NewCommand(ctx, fmt.Sprintf("%s-deploy", stack.Name), &local.CommandArgs{
		Create:   deployCmd,
		Update:   deployCmd,
		Triggers: sdk.ArrayInput(sdk.Array{sdk.String(bundleChecksum)}),
		Environment: sdk.StringMap{
			"CLOUDFLARE_API_TOKEN":  sdk.ToSecret(cfg.Credentials.Credentials).(sdk.StringOutput),
			"CLOUDFLARE_ACCOUNT_ID": sdk.String(cfg.AccountId),
		},
	}, sdk.DependsOn([]sdk.Resource{project}))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to invoke wrangler pages deploy for %q", stack.Name)
	}

	// No domain → serve on <project>.pages.dev only.
	if cfg.Domain == "" {
		params.Log.Info(ctx.Context(),
			"cloudflare pages project %q has no `domain:` set; site will serve at %s.pages.dev",
			projectName, projectName)
		return &api.ResourceOutput{Ref: project}, nil
	}

	// Custom-domain binding + Registrar CNAME.
	params.Log.Info(ctx.Context(), "configure cloudflare pages custom domain %q...", cfg.Domain)
	_, err = cfImpl.NewPagesDomain(ctx, fmt.Sprintf("%s-domain", stack.Name), &cfImpl.PagesDomainArgs{
		AccountId:   sdk.String(cfg.AccountId),
		ProjectName: project.Name,
		Name:        sdk.String(cfg.Domain),
	}, sdk.DependsOn([]sdk.Resource{project}))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to attach cloudflare pages domain %q to project %q", cfg.Domain, projectName)
	}

	if params.Registrar == nil {
		return nil, errors.Errorf(
			"cloudflare-static-website %q sets `domain: %s` but no registrar is configured on this stack — "+
				"add a `resources.registrars.cloudflare` entry on the parent so SC can create the CNAME",
			stack.Name, cfg.Domain,
		)
	}
	cnameTarget := fmt.Sprintf("%s.pages.dev", projectName)
	_, err = params.Registrar.NewRecord(ctx, api.DnsRecord{
		Name:     cfg.Domain,
		Type:     "CNAME",
		ValueOut: sdk.String(cnameTarget).ToStringOutput(),
		Proxied:  true,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to provision CNAME %q → %q", cfg.Domain, cnameTarget)
	}

	return &api.ResourceOutput{Ref: project}, nil
}
