// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/simple-container-com/api/pkg/api/logger"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// The lifted registrar driver's log output must reach the progress stream.
//
// It did not: ProvisionRegistrar handed the driver `logger.New()`, which
// writes to the plugin subprocess's stdout, so every `configure CNAME DNS
// record …` line went nowhere — zero occurrences in forge-notifier run
// 40654637631, the run that verified the baseDnsZone fix. The struct comment
// on cfPlugin.progress already described this adaptor as if it existed, which
// is how the gap survived extraction.
func TestProgressLogger_PublishesDriverOutput(t *testing.T) {
	RegisterTestingT(t)
	ctx := context.Background()

	pub := scPlugin.NewPublisher(0)
	// Compile-time proof it satisfies the interface ProvisionParams.Log wants;
	// the driver is handed exactly this type.
	var log logger.Logger = newProgressLogger(pub, "registrar")

	log.Info(ctx, "configure CNAME DNS record %q for stack %q...", "notifier.simple-forge.com", "forge-notifier")
	log.Warn(ctx, "zone %s is the configured default", "simple-container.com")
	log.Error(ctx, "failed to create DNS record %q: %s", "x.example.com", "boom")
	log.Debug(ctx, "noisy detail")

	batch, ok := pub.Drain()
	Expect(ok).To(BeTrue())
	Expect(batch).To(HaveLen(4))

	// Formatting must be applied, not passed through as a template — the
	// driver calls these with printf args, and an unformatted "%q" in the
	// deploy log is exactly as useless as no line at all.
	Expect(batch[0].GetMessage()).To(Equal(`configure CNAME DNS record "notifier.simple-forge.com" for stack "forge-notifier"...`))
	Expect(batch[0].GetPhase()).To(Equal("registrar"))
	Expect(batch[0].GetSeverity()).To(Equal(scpluginv1.LogSeverity_LOG_SEVERITY_INFO))

	// Severity must survive the adaptor, and this assertion is load-bearing
	// rather than hygiene. SC core's LogStream consumer routes on severity, so
	// an Error arriving as Info is a downgraded failure — and the adaptor makes
	// that strictly worse than the bug it fixes. Before it, an Error and an Info
	// were equally invisible; after it, a mis-severitied Error PRINTS, and reads
	// as progress. The discarded channel carried the driver's Error paths too
	// (`failed to create DNS record` was also 0 occurrences in run 40654637631),
	// which is how a swallowed-provider-error mechanism got posited for the
	// first incident on no evidence at all.
	Expect(batch[1].GetSeverity()).To(Equal(scpluginv1.LogSeverity_LOG_SEVERITY_WARN))
	Expect(batch[2].GetSeverity()).To(Equal(scpluginv1.LogSeverity_LOG_SEVERITY_ERROR))
	Expect(batch[2].GetMessage()).To(Equal(`failed to create DNS record "x.example.com": boom`))
	Expect(batch[3].GetSeverity()).To(Equal(scpluginv1.LogSeverity_LOG_SEVERITY_DEBUG))

	// Nothing may be dropped at this volume — the ring is 1024.
	Expect(pub.Dropped()).To(BeEquivalentTo(0))
}

// The driver calls SetLogLevel/Silent on the logger it is given. Neither may
// filter plugin-side (an event dropped here is unrecoverable), and neither may
// panic or lose the context.
func TestProgressLogger_ContextMutatorsAreInert(t *testing.T) {
	RegisterTestingT(t)
	pub := scPlugin.NewPublisher(0)
	log := newProgressLogger(pub, "registrar")

	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "kept")

	Expect(log.SetLogLevel(ctx, logger.LogLevelSilent).Value(key{})).To(Equal("kept"))
	Expect(log.Silent(ctx).Value(key{})).To(Equal("kept"))

	// Still publishes after a Silent/SetLogLevel round-trip.
	log.Info(log.Silent(ctx), "still here")
	batch, ok := pub.Drain()
	Expect(ok).To(BeTrue())
	Expect(batch).To(HaveLen(1))
	Expect(batch[0].GetMessage()).To(Equal("still here"))
}

// A nil Publisher must not panic. ProvisionRegistrar builds the adaptor from
// p.progress, and a plugin constructed without one (any future test double or
// an embedding that forgets it) would otherwise take down the deploy from a
// log line.
func TestProgressLogger_NilPublisherIsSafe(t *testing.T) {
	RegisterTestingT(t)
	log := newProgressLogger(nil, "registrar")
	Expect(func() { log.Info(context.Background(), "no publisher") }).NotTo(Panic())
}
