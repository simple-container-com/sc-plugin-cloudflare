// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import "github.com/simple-container-com/api/pkg/api"

// TemplateTypeCloudflareStaticWebsite is the `template:` / `type:` token for a
// static site served from Cloudflare Pages via Direct Upload. See
// forge/docs/design/2026-10-31/cloudflare-pages-direct-upload/README.md.
//
// Shape:
//
//	template: cloudflare-static-website
//	config:
//	  credentials: ${auth:cloudflare}
//	  accountId:   ${auth:cloudflare.accountId}
//	  bundleDir:   dist
//	  domain:      www.simple-forge.com   # optional
//	  productionBranch: main               # optional, defaults to "main"
//
// The driver reads these fields and provisions four resources in order:
// PagesProject (Direct Upload — nil Source), local.NewCommand running
// `wrangler pages deploy <bundleDir>`, PagesDomain (if domain), and a
// Registrar CNAME pointing <domain> at <project>.pages.dev.
const TemplateTypeCloudflareStaticWebsite = "cloudflare-static-website"

// PagesConfig is the on-the-wire config for a cloudflare-static-website stack.
//
// Credentials: the Cloudflare API token with scopes `Pages:Edit` + `Account:Read`
// (Pages:Edit covers Project + Deployment + Domain CRUD; Account:Read is
// required by cfImpl.NewPagesProject's AccountId lookup). Mint via the
// Cloudflare dashboard; no SC-side provisioning of the token.
//
// AccountId: Cloudflare account id. Same value as the DNS registrar's accountId,
// which is why both come out of `${auth:cloudflare}`.
//
// BundleDir: path to the pre-built static assets relative to the stack's
// working directory. Mirrors `aws-static-website`'s `bundleDir`. The operator
// is expected to run their own build before `sc deploy` — v1 ships no
// in-plugin bundler.
//
// Domain: optional. If set, the plugin provisions PagesDomain and the Registrar
// CNAME. If empty, the site is reachable only at `<stackName>.pages.dev`.
//
// ProductionBranch: optional, defaults to "main" in the driver. Pages uses this
// as the branch label for direct-upload deployments (bookkeeping only; direct
// upload is not git-tied).
type PagesConfig struct {
	AuthConfig       `json:",inline" yaml:",inline"`
	BundleDir        string `json:"bundleDir"                  yaml:"bundleDir"`
	Domain           string `json:"domain,omitempty"           yaml:"domain,omitempty"`
	ProductionBranch string `json:"productionBranch,omitempty" yaml:"productionBranch,omitempty"`
}

// ReadPagesConfig is the api.ConfigRegisterMap entry for the template, used by
// SC core's server.yaml parser. Mirrors ReadRegistrarConfig.
func ReadPagesConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &PagesConfig{})
}

// OverriddenBaseZone makes PagesConfig satisfy api.DnsConfigAware so a stack
// using a multi-zone cloudflare registrar can tell SC core which base zone
// to look up for the CNAME. Returns "" when no Domain is set (no DNS record
// is provisioned, so no base zone is required).
func (c *PagesConfig) OverriddenBaseZone() string {
	// Return the full domain; SC core's registrar lookup consults
	// DnsPreference.BaseZone and matches by suffix (see
	// pkg/clouds/pulumi/plugin_sidecars.go resolvePluginRegistrar).
	return c.Domain
}
