// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	cfImpl "github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Adopt-on-create for WorkersRoutes.
//
// Both route registrations carry `sdk.RetainOnDelete(true)` (registrar.go), so a
// destroy removes the route from Pulumi state and deliberately leaves it live in
// Cloudflare. A route pattern is unique per zone, so the next deploy plans a
// *create* for a pattern that already exists and Cloudflare refuses it. State
// forgets, the cloud remembers, and the stack is wedged — the retain flag makes
// that a wedge by construction rather than a rare race.
//
// The fleet had not hit the collision only because the create was also being
// dropped between plan and apply (the separate, still-unidentified defect
// tracked as S3 in forge's `cloudflare-plugin-wrong-zone` item). The two were
// masking each other, which is why fixing the drop first would have converted a
// silent no-op into a hard apply failure on every stack holding a retained
// route. Adopting first dissolves that ordering constraint instead of merging
// the two fixes: once a pre-existing route is adopted, there is no collision
// left for the drop fix to expose.
//
// So before registering a route we ask the zone what is already there, and when
// the pattern exists we hand Pulumi `sdk.Import(<id>)` instead of letting it
// plan a create.
//
// **Leaving the import option set permanently is correct, and that is not
// obvious.** The engine decides whether a registration is an import with
// (`pkg/resource/deploy/step_generator.go`, v3.263.0):
//
//	isImport := goal.Custom && goal.ID != "" && (old == nil || old.External || oldImportID != goal.ID)
//
// where `oldImportID` is the state's `ImportID` if set, else its `ID`. Because
// the id we pass is the live route's id, all four states behave:
//
//	in state + live   → oldImportID == goal.ID → not an import → ordinary same/update
//	not in state + live → old == nil           → import, adopted, no collision  ← the wedge
//	not in state + absent → nothing to adopt   → plain create (fresh stack)
//	in state + absent → nothing to adopt       → plain create (re-created out of band)
//
// A fifth case falls out for free: a route in state whose live id differs (it
// was deleted and re-made by hand) re-imports onto the real object rather than
// failing.
//
// Mismatched inputs do not fail the deploy on a modern engine: the import step
// resolves and re-enters step generation with the imported state as `old`
// (`continueResourceImportEvent`), so a route pointing at a different script
// becomes an ordinary update afterwards. We still log the mismatch, because a
// silent repoint of someone's route is worth seeing.
//
// Scope is routes only. The DNS Record is not retained on delete, so it has no
// orphan class to adopt — and inventing one would hide exactly the kind of
// out-of-band record this plugin has already destroyed once.

// liveRoute is a route that exists in the zone right now.
type liveRoute struct {
	id     string
	script string
}

// routeLister reads the routes live in a zone. A seam: the default
// implementation invokes the Cloudflare provider (so it shares the registrar's
// credential and is answered by `pulumi.WithMocks` in tests), and tests swap it
// for a fake.
type routeLister func(ctx *sdk.Context, zoneID string) ([]cfImpl.GetWorkersRoutesResult, error)

// providerRouteLister lists routes through the plugin's own Cloudflare
// provider, the same way the zone itself is looked up in Registrar.
func providerRouteLister(provider *cfImpl.Provider) routeLister {
	return func(ctx *sdk.Context, zoneID string) ([]cfImpl.GetWorkersRoutesResult, error) {
		res, err := cfImpl.LookupWorkersRoutes(ctx, &cfImpl.LookupWorkersRoutesArgs{
			ZoneId: &zoneID,
		}, sdk.Provider(provider))
		if err != nil {
			return nil, err
		}
		return res.Results, nil
	}
}

// liveRoutes returns the zone's routes by pattern, looked up once per deploy.
//
// A lookup failure is logged and treated as "no routes to adopt" rather than
// returned: without adoption we are exactly as good, and as bad, as before this
// code existed, and a missing `Workers Routes Read` scope on the token must not
// be able to fail a deploy that would otherwise succeed. The warning says what
// the operator loses.
func (r *provisioner) liveRoutes(ctx *sdk.Context) map[string]liveRoute {
	r.routesOnce.Do(func() {
		r.routes = map[string]liveRoute{}
		routes, err := r.listRoutes(ctx, r.zone.ZoneId)
		if err != nil {
			r.log.Warn(ctx.Context(),
				"cannot list existing cloudflare worker routes in zone %q, so routes will be registered without adoption — "+
					"a route left behind by an earlier destroy (they are retained on delete) will collide on create: %v",
				r.zone.Name, err)
			return
		}
		for _, route := range routes {
			r.routes[route.Pattern] = liveRoute{id: route.Id, script: route.Script}
		}
		r.log.Debug(ctx.Context(), "zone %q holds %d existing worker route(s)", r.zone.Name, len(r.routes))
	})
	return r.routes
}

// routeOptions returns the resource options for a WorkersRoute registration,
// adding sdk.Import when a route with this pattern already exists in the zone.
func (r *provisioner) routeOptions(ctx *sdk.Context, pattern, script string, base ...sdk.ResourceOption) []sdk.ResourceOption {
	live, exists := r.liveRoutes(ctx)[pattern]
	if !exists {
		return base
	}
	if live.script != script {
		r.log.Warn(ctx.Context(),
			"adopting existing worker route %q (id %s), which currently points at script %q but this stack wants %q — "+
				"the import will be followed by an update repointing it",
			pattern, live.id, live.script, script)
	} else {
		r.log.Info(ctx.Context(), "adopting existing worker route %q (id %s) instead of creating it", pattern, live.id)
	}
	return append(base, sdk.Import(sdk.ID(live.id)))
}
