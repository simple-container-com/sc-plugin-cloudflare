// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

// Why every handler in this file's package must await what it registered.
//
// `cfImpl.NewWorkersRoute` and friends do not send anything to the engine before
// they return. `registerResource` takes its RPC slot synchronously and then does
// the work on a goroutine: it awaits the resource's input Outputs
// (`prepareResourceInputs`) and only then calls `monitor.RegisterResource`
// (`sdk/go/pulumi/context.go`, v3.245.0: `beginRPC()` at :1753, `go func()` at
// :1781, the send at :1855). For a resource with an unresolved input — the route's
// `Script` is `workerScript.ScriptName` — that send happens strictly after the
// constructor returned, and in practice after the whole handler returned.
//
// Pulumi's own programs are safe from this because their entry point drains:
// `RunErr` ends in the context's unexported `wait()`, which blocks until the RPC
// count reaches zero and then returns `ctx.state.rpcError`. This plugin has no
// such entry point. `cmd/`'s `pulumiContextFromHeader` opens a fresh `sdk.Context`
// per gRPC request and each handler `defer done()`s it, which calls
// `Context.Close()` — and `Close()` closes the two gRPC conns and returns
// (`context.go:263-275`). It never waits. `wait()` has exactly three callers
// (`run.go:153`, `provider.go:228`, `provider.go:893`), all in entry points that
// own their context, and none of them is reachable from here.
//
// There is no supported drain to borrow, either. `RunWithContext` is exported and
// does call `ctx.wait()`, but it opens by registering a root `pulumi:pulumi:Stack`
// named `<project>-<stack>` and closes with `RegisterResourceOutputs` — against
// the host's monitor that would inject a second root stack into someone else's
// deployment. `Run`/`RunErr` wrap it and build `RunInfo` from the process
// environment, so they cannot take a monitor address out of a request header.
//
// Two consequences, and they are the two halves of forge-notifier run 40654637631
// (planned `create: 1`, no create in the apply summary, no error, exit 0):
//
//  1. The registration can be dropped. The handler returns nil, SC core reports
//     success, and the engine completes the deployment without ever hearing about
//     the resource.
//  2. A registration that *fails* is silent. The failure is delivered to
//     `ctx.state.rpcError` (`endRPC`, :2855-2861) whose only reader is `wait()`
//     at :295 — unreachable here. So a create the provider refuses is
//     indistinguishable from a create that succeeded.
//
// Awaiting fixes both, and the second half is worth spelling out because it is
// what makes an await-based fix safe. `registerResource`'s goroutine installs
// `defer resState.resolve(ctx, err, ...)` BEFORE it does any work
// (`context.go:1788-1791`), and `resolve` opens by rejecting every one of the
// resource's outputs with that error (`:2312-2318`). So the send's error has two
// destinations: `ctx.state.rpcError`, unreachable without `wait()`, and the
// resource's own outputs. An await is therefore the only reachable reader of the
// error this defect has been swallowing — an awaiting handler gets the real
// `connection is closing` / provider refusal back as a returned error, which is
// the diagnostic that was missing from every run log. And because that `defer` is
// structural rather than an error branch, the await cannot hang on a lost send:
// whatever happens, the outputs get resolved or rejected exactly once.
//
// Note that holding the `sdk.Context` open
// for longer — one context per monitor address instead of one per request — does
// NOT fix either. The handler returns before the send regardless of teardown, so
// a longer-lived context merely lets the late send race SC core, which has already
// been told the handler succeeded; a registration arriving after the engine
// completes the deployment is dropped just as completely and just as silently.
// That change is worth making as defence-in-depth, but behind this one.
//
// Deliberately no timeout, and this is forced rather than merely preferred. A
// deadline would re-introduce exactly the silent drop above the moment a create
// takes longer than it under load — but more to the point it would not work:
// `awaitOnce` checks `ctx.Err()` only BEFORE `o.cond.Wait()` and nothing
// broadcasts that cond on cancellation (`sdk/go/internal/types.go:263-279`), so
// awaiting a never-resolved output is uncancellable. The paragraph above is why
// that is safe here — a registered resource is always resolved or rejected by that
// structural `defer`. It is NOT safe for hand-rolled outputs, so only pass this
// function resources that came from a `cfImpl.New*` call.
//
// One fail-open edge, for whoever extends this: `awaitOnce` on a nil
// `*OutputState` returns unknown with no error (`:264-267`), so a typed-nil
// resource would pass silently rather than erroring. The `res == nil` check below
// catches a nil interface, not a non-nil interface holding a nil pointer.
//
// See forge/docs/design/2026-10-24/sc-plugin-architecture/04-no-destroy-invariant.md
// (Addendum P) and the invariant it adds: a plugin handler must not close a
// pulumi.Context while any resource registered on it is unresolved.

// Ordering matters when a handler registers more than one resource: register them
// ALL first, then await them all. `monitor.RegisterResource` does not return until
// the engine has performed the step, so the await blocks until the create is done
// — awaiting inside a registration loop would serialise creates that the engine
// would otherwise run in parallel (ProvisionRecords' N DNS records turning from
// max(N) into sum(N)). Registering first keeps every RPC in flight and makes the
// awaits a join rather than a queue.

// registered pairs a resource with enough naming to say which of a handler's
// several registrations failed — the SDK's own error does not carry it.
type registered struct {
	kind string
	name string
	res  sdk.Resource
}

// awaitAll joins every registration in order and returns the first error.
func awaitAll(ctx *sdk.Context, rs ...registered) error {
	for _, x := range rs {
		if err := awaitRegistered(ctx, x.kind, x.name, x.res); err != nil {
			return err
		}
	}
	return nil
}

// awaitRegistered blocks until each resource's registration RPC has completed, and
// returns the first registration error. `kind` and `name` only shape the error
// message — they say which of a handler's several resources failed, which the
// SDK's own error does not.
func awaitRegistered(ctx *sdk.Context, kind, name string, res sdk.Resource) error {
	if res == nil {
		return nil
	}
	// URN, not ID — and this is load-bearing, not a style choice. `resolve`
	// publishes the URN unconditionally but gates the id on `keepUnknowns`
	// (`context.go:2321-2326`), and `keepUnknowns` is `|| ctx.DryRun()` (:2294).
	// This plugin's `ctx.DryRun()` always answers false, because
	// `pulumiContextFromHeader` cannot set `RunInfo.DryRun` — the plugin bus's
	// `RequestHeader` carries no dry-run flag (8 fields, none of them), so fixing
	// that needs a proto field in `api` and a host-side change, not an edit here.
	// Consequence: in a preview the monitor's empty id is published as a KNOWN
	// empty string, indistinguishable from a resolved-but-empty id. Any guard of
	// the shape "the id came back empty, so the registration didn't land" is
	// therefore correct on an apply and wrong on every preview. Awaiting the URN
	// sidesteps it entirely — the URN is known in both modes, and it forces the
	// registration just as well.
	if _, err := internals.UnsafeAwaitOutput(ctx.Context(), res.URN()); err != nil {
		return errors.Wrapf(err, "failed to register %s %q", kind, name)
	}
	return nil
}
