// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// The host-override worker's generated JavaScript is the one artefact of this
// plugin that runs on Cloudflare's edge rather than in Pulumi, so no Go test
// can execute it (there is no JS runtime in this module's dependency set, and
// adding one to a shipping plugin for a test is not worth the supply-chain
// surface). These tests therefore pin the *source* of the three defects that
// reached production, each one keyed to a property that must hold for the
// script to be correct.
//
// 2026-11-26 incident this file exists to prevent a repeat of: visitors to
// talk2.simple-forge.ru — a Caddy relay that sets `Host: talk.simple-forge.com`
// and so traverses this worker — were served Cloudflare error 1101 "Worker
// threw exception". Cloudflare's own workersInvocationsAdaptive recorded 54
// scriptThrewException events in 25 minutes against
// `forge-talk--staging...-host-override-script`, *every one with
// subrequests == 0*: the script threw before reaching its own fetch() to the
// origin, which ruled out the origin, CPU limits and SSE stream duration, and
// localised the fault to the `new Request(...)` constructor at the top of
// handleRequest. Reproduced deterministically with
// `curl -X GET --data-raw 'x=1' https://talk.simple-forge.com/alesya.html`
// → HTTP 500 + "Worker threw exception", 5/5. The same zone had 21 routes all
// generated from this one template, so the defect was fleet-wide.

// workerScriptFor renders the worker script exactly as NewOverrideHeaderRule
// hands it to Cloudflare: by capturing the `content` input of the
// WorkerScript resource. Asserting the real resource input (rather than
// re-deriving the template in the test) is what makes these tests meaningful —
// a change to how the script is assembled cannot slip past them.
func workerScriptFor(t *testing.T, rule pApi.OverrideHeaderRule) string {
	t.Helper()

	m := &captureMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		r, err := Registrar(ctx, newRegistrarDescriptor("acct-x", "example.com", nil), pApi.ProvisionParams{Log: logger.New()})
		if err != nil {
			return err
		}
		_, err = r.NewOverrideHeaderRule(ctx, api.Stack{Name: "mystack"}, rule)
		return err
	}, pulumi.WithMocks("test-project", "test-stack", m))
	Expect(err).ToNot(HaveOccurred())

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.captured {
		if a.TypeToken != "cloudflare:index/workerScript:WorkerScript" {
			continue
		}
		content, ok := a.Inputs["content"]
		Expect(ok).To(BeTrue(), "WorkerScript resource must carry a `content` input")
		Expect(content.IsString()).To(BeTrue(), "`content` must resolve to a string, got %v", content)
		return content.StringValue()
	}
	t.Fatal("no cloudflare:index/workerScript:WorkerScript resource was registered")
	return ""
}

func defaultRule() pApi.OverrideHeaderRule {
	return pApi.OverrideHeaderRule{
		Name:     "myservice",
		FromHost: "app.example.com",
		ToHost:   pulumi.String("origin.example.net").ToStringOutput(),
	}
}

// TestWorkerScript_NoBodyOnGetOrHead is the regression gate for the 1101.
//
// Per the Fetch spec, `new Request(url, {method: "GET", body: <non-null>})`
// throws a TypeError. The script forwards the inbound method verbatim, so it
// must not forward an inbound body unconditionally — a GET or HEAD carrying
// one (routine from proxies, crawlers, API clients, `curl -X GET --data`)
// made the constructor throw. Note a GET with `Content-Length: 0` did NOT
// throw, because an empty body is null: only a non-empty body trips it, which
// is why this went unnoticed until a relay put real traffic through the route.
func TestWorkerScript_NoBodyOnGetOrHead(t *testing.T) {
	RegisterTestingT(t)

	script := workerScriptFor(t, defaultRule())

	Expect(script).To(ContainSubstring(`body: methodAllowsBody ? origRequest.body : null`),
		"the Request constructor must drop the body for bodyless methods; passing origRequest.body unconditionally is a TypeError on GET/HEAD and reaches visitors as Cloudflare error 1101")
	Expect(script).To(ContainSubstring(`origRequest.method !== "GET" && origRequest.method !== "HEAD"`),
		"both bodyless methods must be excluded — GET is the common case, HEAD throws identically")

	Expect(script).ToNot(MatchRegexp(`body:\s*origRequest\.body\s*,`),
		"regression: the body is being forwarded unconditionally again. This is the exact 2026-11-26 defect — see this file's header comment.")
}

// TestWorkerScript_HandlerThrowDoesNotBecome1101 pins the defence in depth.
//
// The fault above was a one-line bug, but what turned it into a customer-
// visible outage was that handleRequest's rejection was unhandled: Cloudflare
// renders that as a branded 1101 page, with the thrown message visible only in
// analytics and the origin never contacted. A catch that logs and degrades to
// 502 means the next edge-side defect reads as a gateway error and names
// itself in `wrangler tail`.
func TestWorkerScript_HandlerThrowDoesNotBecome1101(t *testing.T) {
	RegisterTestingT(t)

	script := workerScriptFor(t, defaultRule())

	Expect(script).To(ContainSubstring(".catch(err =>"),
		"handleRequest's promise must be caught; an unhandled rejection is rendered as Cloudflare error 1101")
	Expect(script).To(ContainSubstring("console.error("),
		"the caught error must be logged, or the cause is invisible to `wrangler tail` and the dashboard")
	Expect(script).To(ContainSubstring("status: 502"),
		"a worker-side failure should degrade to 502, not an opaque 1101")

	Expect(script).ToNot(ContainSubstring("event.respondWith(handleRequest(event.request));"),
		"regression: handleRequest is being passed to respondWith with no catch, so any throw becomes a 1101 again")
}

// TestWorkerScript_HeaderGetNullChecks pins the third defect, found while
// diagnosing the first.
//
// `Headers.get()` returns null — never the empty string — for an absent
// header, so the original empty-string comparisons were always true and both
// branches ran on every response. Live effect, observed on talk.simple-forge.com and
// app.simple-forge.com on 2026-11-26: every response carried a literal
// `www-authenticate: null` header, and `content-length` was set to the string
// "null" whenever the origin sent `content-length: 0`.
func TestWorkerScript_HeaderGetNullChecks(t *testing.T) {
	RegisterTestingT(t)

	script := workerScriptFor(t, defaultRule())

	Expect(script).ToNot(ContainSubstring(`!= ''`),
		"regression: comparing Headers.get() against '' is always true, because an absent header reads as null — that is what set `www-authenticate: null` on every live response")

	Expect(script).To(ContainSubstring("const remappedAuth = response.headers.get(\"x-amzn-remapped-www-authenticate\");"),
		"read the header once into a local, then test it for presence")
	Expect(script).To(ContainSubstring("if (remappedAuth) {"),
		"www-authenticate must only be appended when the remapped header is actually present")
	Expect(script).To(ContainSubstring("const xContentLength = response.headers.get(\"x-content-length\");"),
		"read the header once into a local, then test it for presence")
	Expect(script).To(ContainSubstring(`response.headers.get("content-length") == "0" && xContentLength`),
		"content-length must only be overridden when x-content-length carries a value")
}

// TestWorkerScript_PreservesHostOverrideBehaviour guards against fixing the
// bugs above by accident-of-rewrite: the script's actual purpose — rewriting
// the hostname to the override target and telling the origin which host the
// visitor asked for — must survive. The X-Forwarded-Host contract in
// particular is load-bearing for every SC service that serves more than one
// domain, and `set` (not `append`) is what lets the origin trust it.
func TestWorkerScript_PreservesHostOverrideBehaviour(t *testing.T) {
	RegisterTestingT(t)

	script := workerScriptFor(t, defaultRule())

	Expect(script).To(ContainSubstring("url.hostname = overrideHost;"))
	Expect(script).To(ContainSubstring(`fwdHeaders.set("X-Forwarded-Host", origHost);`),
		"set(), never append(): a client-supplied X-Forwarded-Host must be replaced so the origin can trust the value")
	Expect(script).To(ContainSubstring(`redirect: "manual"`),
		"3xx responses must not be followed by the worker")
	Expect(script).To(ContainSubstring("const overrideHost = \"origin.example.net\";"),
		"the rule's ToHost must be interpolated into the script")
}

// TestWorkerScript_BasicAuthVariantStillGuarded — the BasicAuth path rewrites
// pagesCode and appends helper functions, i.e. it produces a materially
// different script. The body fix sits above the injection point, so it must
// hold for this variant too; this test exists because a future refactor that
// moves the Request construction into the per-variant code would silently
// reintroduce the 1101 for authenticated hosts only.
func TestWorkerScript_BasicAuthVariantStillGuarded(t *testing.T) {
	RegisterTestingT(t)

	rule := defaultRule()
	rule.BasicAuth = &pApi.BasicAuth{
		Username: "user",
		Password: "pass",
		Realm:    "test",
	}

	script := workerScriptFor(t, rule)

	Expect(script).To(ContainSubstring(`body: methodAllowsBody ? origRequest.body : null`),
		"the GET/HEAD body fix must apply to the BasicAuth variant as well")
	Expect(script).To(ContainSubstring(".catch(err =>"),
		"the 1101 guard must apply to the BasicAuth variant as well")
	Expect(script).To(ContainSubstring("getUnauthorizedResponse"),
		"sanity: this is in fact the BasicAuth variant")
	Expect(strings.Count(script, "const methodAllowsBody")).To(Equal(1),
		"exactly one Request construction site, so one place can get this wrong")
}
