// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"github.com/simple-container-com/api/pkg/api"
)

const ProviderType = "cloudflare"

func init() {
	api.RegisterProviderConfig(api.ConfigRegisterMap{
		// cloudflare
		RegistrarType:                       ReadRegistrarConfig,
		TemplateTypeCloudflareStaticWebsite: ReadTemplateConfig,
	})

	// Client-side converter: a `type: static` client stack whose parent
	// template resolves to `cloudflare-static-website` gets its merged
	// *PagesInput built here. Mirrors sc-plugin-aws's ToStaticSiteConfig
	// registration.
	api.RegisterCloudStaticSiteConverter(api.CloudStaticSiteConfigRegister{
		TemplateTypeCloudflareStaticWebsite: ToPagesConfig,
	})

	// Fool-protection: when SC core has dropped the in-tree cloudflare
	// subtree, a server.yaml that uses `type: cloudflare` but lacks a
	// `plugins.cloudflare` block would silently fail at provision time.
	// This registration lets SC core's ValidatePluginRequirements emit a
	// copy-paste-ready error at server.yaml parse time instead.
	api.RegisterPluginRequirement(api.PluginRequirement{
		PluginName: "cloudflare",
		OwnedTypes: []string{
			RegistrarType,
			TemplateTypeCloudflareStaticWebsite,
		},
		DefaultSource: api.PluginSource{
			Type:       "github-release",
			Repository: "simple-container-com/sc-plugin-cloudflare",
		},
	})
}
