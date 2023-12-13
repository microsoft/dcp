// Copyright (c) Microsoft Corporation. All rights reserved.

package telemetry

import (
	"context"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const suppressIfSuccessful = "suppressIfSuccessful"

type suppressIfSuccessfulSpanProcessor struct {
	innerSpanProcessor sdktrace.SpanProcessor
}

func (p *suppressIfSuccessfulSpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.innerSpanProcessor.OnStart(ctx, s)
}

func (p *suppressIfSuccessfulSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	suppressIfSuccessful := slices.Any(s.Attributes(), func(attr attribute.KeyValue) bool {
		return attr.Key == suppressIfSuccessful && attr.Valid() && attr.Value.AsBool()
	})

	if !suppressIfSuccessful || s.Status().Code == codes.Error {
		p.innerSpanProcessor.OnEnd(s)
	}
}

func (p *suppressIfSuccessfulSpanProcessor) Shutdown(ctx context.Context) error {
	return p.innerSpanProcessor.Shutdown(ctx)
}

func (p *suppressIfSuccessfulSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.innerSpanProcessor.ForceFlush(ctx)
}

func NewSuppressIfSuccessfulSpanProcessor(innerSpanProcessor sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return &suppressIfSuccessfulSpanProcessor{
		innerSpanProcessor: innerSpanProcessor,
	}
}
