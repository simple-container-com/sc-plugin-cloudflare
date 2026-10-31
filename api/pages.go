// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
)

// TemplateTypeCloudflareStaticWebsite is the `type:` discriminator the plugin
// advertises for a static site served from Cloudflare Pages via Direct
// Upload. See forge/docs/design/2026-10-31/cloudflare-pages-direct-upload/.
//
// Shape in the parent server.yaml `templates:` block:
//
//	templates:
//	  cloudflare-static-website:
//	    type: cloudflare-static-website
//	    config:
//	      credentials: ${secret:CLOUDFLARE_API_TOKEN}
//	      accountId:   e2207ddedc3cfe4e321f72b07e0592b5
//
// Shape in the client stack `client.yaml`:
//
//	stacks:
//	  staging:
//	    type: static
//	    template: cloudflare-static-website
//	    config:
//	      bundleDir: ${git:root}/sites/<name>/public
//	      domain: <name>.example.com
//	      baseDnsZone: example.com
//	      indexDocument: index.html
//	      errorDocument: index.html
//	      productionBranch: main    # optional
const TemplateTypeCloudflareStaticWebsite = "cloudflare-static-website"

// TemplateConfig is the parent-side template config for Pages — exactly the
// cloudflare account coordinates. Mirrors sc-plugin-aws/api/aws.go
// `TemplateConfig` which embeds `AccountConfig`; here `AuthConfig` plays the
// same role (same fields: Credentials + AccountId).
type TemplateConfig struct {
	AuthConfig `json:",inline" yaml:",inline"`
}

// ReadTemplateConfig is the api.ConfigRegisterMap entry for the template
// type, used by SC core's server.yaml parser to decode the `templates:` block.
func ReadTemplateConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &TemplateConfig{})
}

// PagesInput is the merged shape the plugin's Provision handler receives at
// deploy-time: the parent-side TemplateConfig (credentials + accountId) plus
// the client-side StackConfigStatic (bundleDir + domain + baseDnsZone + index
// / error documents). Mirrors sc-plugin-aws's StaticSiteInput exactly.
//
// ProductionBranch is a Pages-only knob (branch label for direct-upload
// deployments); defaults to "main" in the driver when empty.
type PagesInput struct {
	TemplateConfig        `json:"templateConfig" yaml:"templateConfig"`
	api.StackConfigStatic `json:",inline"        yaml:",inline"`
	StackDir              string `json:"stackDir"                   yaml:"stackDir"`
	StackName             string `json:"stackName"                  yaml:"stackName"`
	ProductionBranch      string `json:"productionBranch,omitempty" yaml:"productionBranch,omitempty"`

	// Flat mirrors of StackConfigStatic.Site.* — populated by
	// ToPagesConfig from stackCfg.Site. The wire-decode gap: `Site
	// StaticSiteConfig` is a NAMED field with `json:",inline"`, but
	// Go's encoding/json ignores the ",inline" option on a named
	// (non-anonymous) field, so SC core's `serializeConfigToInputs`
	// serialises `"Site": {...}` nested. The plugin's `decodeInputsAs`
	// then does `yaml.Marshal(plain) + yaml.Unmarshal(bytes, out)`,
	// and the yaml library treats `Site:` as a KEY while the Go
	// struct expects the inner fields at the PARENT level — so the
	// `Site:` subtree is dropped silently and `cfg.Site.Domain` is
	// "". Live-caught 2026-10-31 on navizon-landing: PagesProject
	// created without a `domain:` binding, old aws CNAME deleted, no
	// new CNAME, navizon.atriumdev.app public DNS broke until this
	// field was wired. Fix is pragmatic rather than fundamental:
	// expose the three Site.* fields the Pages driver reads at the
	// top of PagesInput, populate from stackCfg.Site in
	// ToPagesConfig, and read these in StaticWebsite with a
	// stackCfg.Site.* fallback for callers who build PagesInput by
	// hand. A proper fix lives in the api repo — flatten the
	// Site inline round-trip; see 04 §V.
	Domain        string `json:"domain,omitempty"        yaml:"domain,omitempty"`
	BaseDnsZone   string `json:"baseDnsZone,omitempty"   yaml:"baseDnsZone,omitempty"`
	IndexDocument string `json:"indexDocument,omitempty" yaml:"indexDocument,omitempty"`
	ErrorDocument string `json:"errorDocument,omitempty" yaml:"errorDocument,omitempty"`
}

// OverriddenBaseZone implements api.DnsConfigAware so the Cloudflare registrar
// uses the stack's own baseDnsZone instead of the parent registrar's default.
// Prefers the top-level BaseDnsZone (populated by ToPagesConfig) with a
// Site.BaseDnsZone fallback for callers who construct PagesInput directly.
func (i *PagesInput) OverriddenBaseZone() string {
	if i.BaseDnsZone != "" {
		return i.BaseDnsZone
	}
	return i.Site.BaseDnsZone
}

// EffectiveDomain returns the custom domain to bind, with the top-level
// Domain field preferred over Site.Domain (the latter is dropped on the
// plugin bus — see PagesInput.Domain for the full story).
func (i *PagesInput) EffectiveDomain() string {
	if i.Domain != "" {
		return i.Domain
	}
	return i.Site.Domain
}

// ToPagesConfig is the client-side converter registered under
// api.RegisterCloudStaticSiteConverter — SC core calls it after resolving the
// parent template config and client stack config, so the plugin receives one
// merged struct. Mirrors sc-plugin-aws's ToStaticSiteConfig.
func ToPagesConfig(tpl any, stackDir, stackName string, stackCfg *api.StackConfigStatic) (any, error) {
	templateCfg, ok := tpl.(*TemplateConfig)
	if !ok {
		return nil, errors.Errorf("template config is not of type cloudflare.TemplateConfig (got %T)", tpl)
	}
	if templateCfg == nil {
		return nil, errors.New("template config is nil")
	}
	if stackCfg == nil {
		return nil, errors.New("stack config is nil")
	}
	return &PagesInput{
		TemplateConfig:    *templateCfg,
		StackConfigStatic: *stackCfg,
		StackDir:          stackDir,
		StackName:         stackName,
		// Flat mirrors — see field docs. Carry Site.* out to the top
		// so the json+yaml round-trip on the plugin bus preserves them.
		Domain:        stackCfg.Site.Domain,
		BaseDnsZone:   stackCfg.Site.BaseDnsZone,
		IndexDocument: stackCfg.Site.IndexDocument,
		ErrorDocument: stackCfg.Site.ErrorDocument,
	}, nil
}
