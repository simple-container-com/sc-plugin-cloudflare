// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"fmt"

	"github.com/simple-container-com/api/pkg/api/logger"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// progressLogger adapts api's logger.Logger onto the plugin's progress
// Publisher, so everything the lifted registrar driver logs reaches the
// operator's deploy log instead of a void.
//
// It needs to exist because the two halves of this plugin log through
// different channels. The gRPC handlers in main.go push to the Publisher,
// which SC core drains over LogStream and prints as
// `[plugin cloudflare/<phase>] …`. The lifted internal/*.go driver logs
// through the `logger.Logger` handed to it on ProvisionParams.Log — and
// that was `logger.New()`, which writes to the plugin SUBPROCESS's stdout.
// Under hashicorp/go-plugin that is not the operator's terminal, and with
// the plugin's own hclog pinned at Warn (main.go) the Info-level lines are
// dropped outright. Measured on forge-notifier run 40654637631: the driver
// emits `configure CNAME DNS record …` and two `configure cloudflare worker
// script …` lines on the endpoint path, and ZERO of them appear anywhere in
// the deploy log.
//
// That silence is what made the wrong-zone defect invisible, and it set a
// second trap worth spelling out: **a progress phase with no lines does not
// mean nothing happened.** The `worker-route` and `dns-record` phases are
// emitted only by the gRPC handlers of the same name, but
// ProvisionDomainForEndpoint reaches NewRecord and NewOverrideHeaderRule as
// plain Go calls (internal/registrar.go:139,149). So an endpoint deploy
// registers a Record and two WorkersRoutes while both of those phases stay
// empty — which reads as "the handler never ran, so who registered the
// route?". Routing the driver's own logger here is what closes that gap,
// rather than hand-adding progress calls at each resource site and hoping
// the next one remembers.
type progressLogger struct {
	pub   *scPlugin.Publisher
	phase string
}

// newProgressLogger returns a logger.Logger that publishes under phase.
func newProgressLogger(pub *scPlugin.Publisher, phase string) logger.Logger {
	return &progressLogger{pub: pub, phase: phase}
}

func (l *progressLogger) push(sev scpluginv1.LogSeverity, format string, a ...any) {
	if l.pub == nil {
		return
	}
	l.pub.Push(sev, l.phase, fmt.Sprintf(format, a...))
}

func (l *progressLogger) Error(_ context.Context, format string, a ...any) {
	l.push(scpluginv1.LogSeverity_LOG_SEVERITY_ERROR, format, a...)
}

func (l *progressLogger) Warn(_ context.Context, format string, a ...any) {
	l.push(scpluginv1.LogSeverity_LOG_SEVERITY_WARN, format, a...)
}

func (l *progressLogger) Info(_ context.Context, format string, a ...any) {
	l.push(scpluginv1.LogSeverity_LOG_SEVERITY_INFO, format, a...)
}

func (l *progressLogger) Debug(_ context.Context, format string, a ...any) {
	l.push(scpluginv1.LogSeverity_LOG_SEVERITY_DEBUG, format, a...)
}

// SetLogLevel and Silent are context mutators on the interface. Severity
// filtering belongs to SC core's logger on the consuming end of LogStream,
// not here: dropping an event plugin-side makes it unrecoverable, and the
// whole point of this type is that driver output stopped going missing.
func (l *progressLogger) SetLogLevel(ctx context.Context, _ int) context.Context {
	return ctx
}

func (l *progressLogger) Silent(ctx context.Context) context.Context {
	return ctx
}
