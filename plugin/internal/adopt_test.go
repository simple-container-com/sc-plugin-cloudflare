// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	cfImpl "github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// Adopt-on-create for WorkersRoutes — see adopt.go for why this exists.
//
// These tests assert adoption *end to end* rather than by counting opaque
// ResourceOptions: `pulumi.WithMocks` fills `MockResourceArgs.ID` from the
// registration's import id (`sdk/go/pulumi/mocks.go`, v3.263.0:
// `ID: in.GetImportId()`). So a non-empty ID on the captured WorkersRoute
// registration *is* the engine seeing an import, and an empty one is a plain
// create. Nothing else sets that field for a mocked resource.

const workersRouteType = "cloudflare:index/workersRoute:WorkersRoute"

// routeImportIDs maps each captured WorkersRoute registration from its pattern
// to the import id the engine was handed ("" when none).
func (m *captureMocks) routeImportIDs() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, a := range m.captured {
		if a.TypeToken != workersRouteType {
			continue
		}
		out[a.Inputs["pattern"].StringValue()] = a.ID
	}
	return out
}

// fakeLister returns a routeLister serving the given routes, and a counter of
// how many times it was called — the lookup is behind a sync.Once, so "once per
// deploy regardless of how many routes get registered" is part of the contract.
func fakeLister(routes ...cfImpl.GetWorkersRoutesResult) (routeLister, *int) {
	calls := 0
	return func(ctx *pulumi.Context, zoneID string) ([]cfImpl.GetWorkersRoutesResult, error) {
		calls++
		return routes, nil
	}, &calls
}

func liveRouteResult(pattern, id, script string) cfImpl.GetWorkersRoutesResult {
	return cfImpl.GetWorkersRoutesResult{Id: id, Pattern: pattern, Script: script}
}

// withFakeLister runs fn against a registrar whose route lookup is replaced,
// returning the captured registrations.
func withFakeLister(t *testing.T, lister routeLister, fn func(ctx *pulumi.Context, prov *provisioner) error) *captureMocks {
	t.Helper()
	m := &captureMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		r, err := Registrar(ctx, newRegistrarDescriptor("acct-x", "example.com", nil), pApi.ProvisionParams{Log: logger.New()})
		if err != nil {
			return err
		}
		prov := r.(*provisioner)
		// The seam: Registrar wires providerRouteLister, which would invoke the
		// real provider data source.
		prov.listRoutes = lister
		return fn(ctx, prov)
	}, pulumi.WithMocks("test-project", "test-stack", m))
	Expect(err).ToNot(HaveOccurred())
	return m
}

// A route left behind by an earlier destroy (they are retained on delete) must
// be adopted, not re-created. Without this the next deploy plans a create for a
// pattern that already exists and Cloudflare refuses it — a route pattern is
// unique per zone.
func TestAdopt_ExistingRouteIsImported(t *testing.T) {
	RegisterTestingT(t)

	// The real fixture this was built against: pattern
	// notifier.simple-forge.com/*, id 563d29dd3d564d7fb8b53d4dd39214ba, live in
	// Cloudflare and absent from Pulumi state.
	lister, calls := fakeLister(liveRouteResult("api.example.com/*", "563d29dd3d564d7fb8b53d4dd39214ba", "my-worker"))

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.NewWorkerScript(ctx, "my-worker", "api.example.com", "// script")
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{
		"api.example.com/*": "563d29dd3d564d7fb8b53d4dd39214ba",
	}), "an existing route must be registered with sdk.Import(<live id>) so the engine adopts it instead of creating a duplicate pattern")
	Expect(*calls).To(Equal(1))
}

// The other call site. Both registrations go through routeOptions; a fix that
// covers only NewWorkerScript leaves every override-header rule wedged.
func TestAdopt_ExistingRouteIsImported_OverrideHeaderRule(t *testing.T) {
	RegisterTestingT(t)

	lister, _ := fakeLister(liveRouteResult("app.example.com/*", "route-id-override", "mystackmyservice-host-override-script"))

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.NewOverrideHeaderRule(ctx, api.Stack{Name: "mystack"}, pApi.OverrideHeaderRule{
			Name:     "myservice",
			FromHost: "app.example.com",
			ToHost:   pulumi.String("origin.example.net").ToStringOutput(),
		})
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{
		"app.example.com/*": "route-id-override",
	}))
}

// A fresh stack in a zone holding unrelated routes must still plan a plain
// create. Adoption keys on the exact pattern, and matching anything looser
// would hand the stack someone else's route.
func TestAdopt_AbsentRouteIsCreated(t *testing.T) {
	RegisterTestingT(t)

	lister, calls := fakeLister(
		liveRouteResult("other.example.com/*", "unrelated-id", "someone-elses-worker"),
		// Deliberately close to, but not, the pattern we register.
		liveRouteResult("api.example.com/?", "near-miss-id", "near-miss-worker"),
	)

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.NewWorkerScript(ctx, "my-worker", "api.example.com", "// script")
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{"api.example.com/*": ""}),
		"a pattern that does not already exist must be a plain create — no import id")
	Expect(*calls).To(Equal(1))
}

// A lookup failure must not fail the deploy. Without adoption we are exactly as
// good, and as bad, as before adopt.go existed; a token missing the `Workers
// Routes Read` scope must not be able to break a deploy that would otherwise
// succeed. It falls back to create, and warns.
func TestAdopt_ListerErrorFallsBackToCreate(t *testing.T) {
	RegisterTestingT(t)

	calls := 0
	lister := routeLister(func(ctx *pulumi.Context, zoneID string) ([]cfImpl.GetWorkersRoutesResult, error) {
		calls++
		return nil, fmt.Errorf("403 Authentication error (10000)")
	})

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.NewWorkerScript(ctx, "my-worker", "api.example.com", "// script")
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{"api.example.com/*": ""}))
	// Still only one attempt — a failing lookup must not be retried per route.
	Expect(calls).To(Equal(1))
}

// A route whose live script differs from the one this stack wants is still
// adopted. The import step resolves and re-enters step generation with the
// imported state as `old` (`continueResourceImportEvent`), so the mismatch
// becomes an ordinary update repointing the route — refusing to adopt here
// would leave the stack wedged on the collision instead.
func TestAdopt_ScriptMismatchStillAdopts(t *testing.T) {
	RegisterTestingT(t)

	lister, _ := fakeLister(liveRouteResult("api.example.com/*", "stale-route-id", "a-previous-worker"))

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.NewWorkerScript(ctx, "my-worker", "api.example.com", "// script")
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{"api.example.com/*": "stale-route-id"}))
}

// The zone is listed once per deploy, not once per route. Two registrations,
// one adopted and one created, share the single sync.Once lookup.
func TestAdopt_ZoneIsListedOncePerDeploy(t *testing.T) {
	RegisterTestingT(t)

	lister, calls := fakeLister(liveRouteResult("app.example.com/*", "route-id-override", "mystackmyservice-host-override-script"))

	m := withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		if _, err := prov.NewWorkerScript(ctx, "my-worker", "api.example.com", "// script"); err != nil {
			return err
		}
		_, err := prov.NewOverrideHeaderRule(ctx, api.Stack{Name: "mystack"}, pApi.OverrideHeaderRule{
			Name:     "myservice",
			FromHost: "app.example.com",
			ToHost:   pulumi.String("origin.example.net").ToStringOutput(),
		})
		return err
	})

	Expect(m.routeImportIDs()).To(Equal(map[string]string{
		"api.example.com/*": "",
		"app.example.com/*": "route-id-override",
	}))
	Expect(*calls).To(Equal(1))
}

// A stack with no endpoints must not list the zone at all — the lookup is
// lazy, so adding it costs nothing on a registrar-only deploy (and cannot fail
// one).
func TestAdopt_NoRoutesNoLookup(t *testing.T) {
	RegisterTestingT(t)

	lister, calls := fakeLister()

	withFakeLister(t, lister, func(ctx *pulumi.Context, prov *provisioner) error {
		_, err := prov.ProvisionRecords(ctx, pApi.ProvisionParams{Log: logger.New()})
		return err
	})

	Expect(*calls).To(BeZero())
}
