// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// These tests cover the defect that took forge-notifier's route out of a release
// (run 40654637631): a planned `create` was absent from the apply summary, with no
// error and exit 0.
//
// Every other test in this package drives the registrar through `pulumi.RunErr`,
// which ends by calling the context's unexported `wait()` — the RPC drain. That
// drain is what makes those tests pass, and it is also why they could never see
// this bug: the plugin's real entry point does NOT drain. `cmd/`'s
// `pulumiContextFromHeader` opens a fresh `sdk.Context` per gRPC request and each
// handler `defer done()`s it, which calls `Context.Close()` — and `Close()` closes
// the two gRPC conns and returns (`sdk/go/pulumi/context.go`, v3.245.0). It does
// not wait. `wait()` has exactly three callers, all in entry points that own their
// context (`run.go:153`, `provider.go:228`, `provider.go:893`).
//
// So these tests build the context the way `cmd/` builds it — `pulumi.NewContext`
// with `RunInfo.Mocks` — rather than the way the other tests do. `NewContext`
// honours `RunInfo.Mocks` (`context.go:137`) and leaves both conns nil, which
// means `Close()` here is a literal no-op. That is deliberate and it sharpens the
// test: the registration is not lost to teardown, it is lost because
// `registerResource` takes its RPC slot synchronously and then sends on a
// goroutine (`context.go:1753` then `:1781`), so a resource with an unresolved
// input is sent *after* the handler has already returned success to its caller.
// Teardown only decides whether the late send also fails. The assertion is
// therefore on the handler's return, not on Close().
//
// The invariant under test: a handler must not return until every resource it
// registered has reached the monitor — and if one failed, it must return that
// error rather than nil. See
// forge/docs/design/2026-10-24/sc-plugin-architecture/04-no-destroy-invariant.md,
// Addendum P.

const (
	workerScriptType = "cloudflare:index/workerScript:WorkerScript"
	recordType       = "cloudflare:index/record:Record"
)

// slowMocks delays the create of one resource type. It models the real timing:
// the route's `Script` input is `workerScript.ScriptName`, so the route's
// registration cannot be sent until the script's create has resolved.
type slowMocks struct {
	captureMocks
	slowType string
	delay    time.Duration
	failType string
}

func (m *slowMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if args.TypeToken == m.slowType {
		time.Sleep(m.delay)
	}
	if args.TypeToken == m.failType {
		// Record the attempt before failing it, so a test can tell "the
		// registration never happened" apart from "it happened and failed".
		_, _, _ = m.captureMocks.NewResource(args)
		return "", nil, errAssertedCreateFailure
	}
	return m.captureMocks.NewResource(args)
}

var errAssertedCreateFailure = errors.New("cloudflare refused the create (asserted by test)")

func (m *captureMocks) typeTokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.captured))
	for _, a := range m.captured {
		out = append(out, a.TypeToken)
	}
	sort.Strings(out)
	return out
}

// requestContext builds the context the plugin's gRPC layer builds: fresh per
// request, mocks attached, no drain on close.
func requestContext(t *testing.T, m pulumi.MockResourceMonitor) *pulumi.Context {
	t.Helper()
	pctx, err := pulumi.NewContext(context.Background(), pulumi.RunInfo{
		Project: "test-project",
		Stack:   "test-stack",
		Mocks:   m,
	})
	Expect(err).ToNot(HaveOccurred())
	t.Cleanup(func() { _ = pctx.Close() })
	return pctx
}

// TestAwait_EndpointHandlerDoesNotReturnBeforeItsRoute is the falsifying
// experiment named in the roadmap item. The worker script's create is made slow,
// which is exactly the condition the route's input depends on. When
// ProvisionDomainForEndpoint returns, all three of the resources it registered
// must already have reached the monitor.
//
// Before the fix this fails with only the DNS record present: the handler returns
// `record, nil` while the script is still inside its create and the route has not
// been sent at all. That is the production symptom — a planned create missing from
// the apply with exit 0.
func TestAwait_EndpointHandlerDoesNotReturnBeforeItsRoute(t *testing.T) {
	RegisterTestingT(t)

	m := &slowMocks{slowType: workerScriptType, delay: 300 * time.Millisecond}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", nil),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	_, err = reg.ProvisionDomainForEndpoint(pctx, api.Stack{Name: "mystack"}, pApi.DomainEndpoint{
		Name:       "myservice",
		Domain:     "api.example.com",
		TargetHost: pulumi.String("origin.example.net").ToStringOutput(),
	})
	Expect(err).ToNot(HaveOccurred())

	Expect(m.typeTokens()).To(ContainElements(recordType, workerScriptType, workersRouteType),
		"ProvisionDomainForEndpoint returned before every resource it registered had reached the monitor. "+
			"The handler's caller treats a nil error as 'all three exist'; SC core then reports success and the "+
			"engine completes the deployment without the route. Await the registrations before returning.")
}

// TestAwait_WorkerScriptHandlerAwaitsItsRoute is the same invariant at the other
// call site, reached directly rather than through the endpoint handler.
func TestAwait_WorkerScriptHandlerAwaitsItsRoute(t *testing.T) {
	RegisterTestingT(t)

	m := &slowMocks{slowType: workerScriptType, delay: 300 * time.Millisecond}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", nil),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	_, err = reg.(*provisioner).NewWorkerScript(pctx, "my-worker", "api.example.com", "// script")
	Expect(err).ToNot(HaveOccurred())

	Expect(m.typeTokens()).To(ContainElements(workerScriptType, workersRouteType),
		"NewWorkerScript returned before its script and route had reached the monitor")
}

// TestAwait_RouteCreateFailureIsReturned covers the other half of the same
// defect, and it is the half that made the incident hard to read. A create that
// the provider *rejects* must come back as an error from the handler. Today the
// failure is delivered to `ctx.state.rpcError` (`endRPC`, context.go:2855), whose
// only reader is the unexported `wait()` the plugin never calls — so the handler
// has already returned nil and the error is discarded. Hence: planned create,
// no create in the apply, no error, exit 0.
func TestAwait_RouteCreateFailureIsReturned(t *testing.T) {
	RegisterTestingT(t)

	m := &slowMocks{failType: workersRouteType}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", nil),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	_, err = reg.(*provisioner).NewWorkerScript(pctx, "my-worker", "api.example.com", "// script")
	Expect(err).To(HaveOccurred(),
		"a route whose create the provider refused must surface as an error from the handler. "+
			"Returning nil here is what turns a refused create into a silent no-op with exit 0.")
	Expect(m.typeTokens()).To(ContainElement(workersRouteType),
		"the route's create should have been attempted")
}

// TestAwait_RecordHandlerAwaitsItsRecord — NewRecord is the one handler whose
// resource has no slow sibling, so it is the control: it must hold the invariant
// too, and it must keep holding it if someone later makes the record depend on an
// Output (it already can — `dnsRecord.ValueOut`).
func TestAwait_RecordHandlerAwaitsItsRecord(t *testing.T) {
	RegisterTestingT(t)

	// Slow the record itself: the handler must still not return early.
	m := &slowMocks{slowType: recordType, delay: 200 * time.Millisecond}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", nil),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	_, err = reg.(*provisioner).NewRecord(pctx, api.DnsRecord{
		Name: "api", Type: "CNAME", Value: "origin.example.net",
	})
	Expect(err).ToNot(HaveOccurred())

	Expect(m.typeTokens()).To(ContainElement(recordType),
		"NewRecord returned before the record had reached the monitor")
}

// TestAwait_ProvisionRecordsAwaitsEveryRecord — ProvisionRecords registers N
// records in a loop and returns `Ref: sdk.ToArrayOutput(res)`. All N must have
// reached the monitor when it returns, not just the ones that happened to win the
// race.
func TestAwait_ProvisionRecordsAwaitsEveryRecord(t *testing.T) {
	RegisterTestingT(t)

	records := []api.DnsRecord{
		{Name: "@", Type: "A", Value: "192.0.2.1"},
		{Name: "www", Type: "CNAME", Value: "example.com"},
		{Name: "api", Type: "A", Value: "192.0.2.3"},
	}
	m := &slowMocks{slowType: recordType, delay: 100 * time.Millisecond}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", records),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	_, err = reg.ProvisionRecords(pctx, pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	Expect(m.namesByType(recordType)).To(HaveLen(len(records)),
		"ProvisionRecords returned before all of its records had reached the monitor")
}

// TestAwait_ProvisionRecordsStillCreatesInParallel guards the half of the fix that
// is invisible in the code: ProvisionRecords registers all of its records and
// *then* awaits them, in two phases. Collapsing that into one loop — await each
// record right after registering it — would still pass every other test in this
// file while turning N parallel creates into N sequential ones, because
// `monitor.RegisterResource` does not return until the engine has performed the
// step. With five records at 200ms each, serial is ~1s and parallel is ~200ms, so
// the threshold below has a wide margin and only trips on a real regression.
func TestAwait_ProvisionRecordsStillCreatesInParallel(t *testing.T) {
	RegisterTestingT(t)

	const (
		perRecord = 200 * time.Millisecond
		count     = 5
	)
	records := make([]api.DnsRecord, 0, count)
	for i := range count {
		records = append(records, api.DnsRecord{
			Name:  fmt.Sprintf("host%d", i),
			Type:  "A",
			Value: fmt.Sprintf("192.0.2.%d", i+1),
		})
	}

	m := &slowMocks{slowType: recordType, delay: perRecord}
	pctx := requestContext(t, m)

	reg, err := Registrar(pctx, newRegistrarDescriptor("acct-await", "example.com", records),
		pApi.ProvisionParams{Log: logger.New()})
	Expect(err).ToNot(HaveOccurred())

	start := time.Now()
	_, err = reg.ProvisionRecords(pctx, pApi.ProvisionParams{Log: logger.New()})
	elapsed := time.Since(start)

	Expect(err).ToNot(HaveOccurred())
	Expect(m.namesByType(recordType)).To(HaveLen(count))
	Expect(elapsed).To(BeNumerically("<", count*perRecord/2),
		"ProvisionRecords took %s for %d records of %s each — that is serial, so the registration "+
			"and await phases have been interleaved. Register every record first, then await the batch.",
		elapsed, count, perRecord)
}
