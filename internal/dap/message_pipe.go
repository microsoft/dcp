/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/syncmap"
)

// MessagePipe provides a FIFO message queue with a dedicated writer goroutine
// that assigns monotonically increasing sequence numbers to messages as they
// are dequeued. This guarantees that sequence numbers on the wire are always
// in-order, even when multiple goroutines enqueue messages concurrently.
//
// Each pipe owns a SeqCounter (atomic, shared with shutdown writers) and a
// seqMap for tracking virtualSeq→originalSeq mappings so that response
// correlation can be performed by the opposite direction's reader.
type MessagePipe struct {
	// transport is the write destination for messages.
	transport Transport

	// ch is the unbounded channel used as the FIFO queue.
	ch *concurrency.UnboundedChan[*MessageEnvelope]

	// SeqCounter generates monotonically increasing sequence numbers.
	// It is atomic so that shutdown code can continue assigning seq values
	// after the writer goroutine has stopped.
	SeqCounter atomic.Int64

	// seqMap maps bridge-assigned sequence numbers to original sequence numbers.
	// For the adapter-bound pipe, this maps virtualSeq→originalIDESeq so that
	// the adapter-to-IDE reader can restore request_seq on responses.
	seqMap syncmap.Map[int, int]

	// log is the logger for this pipe.
	log logr.Logger

	// name identifies this pipe in log messages (e.g., "adapterPipe", "idePipe").
	name string
}

// NewMessagePipe creates a new MessagePipe that writes to the given transport.
// The pipe's internal goroutine (for the UnboundedChan) is bound to ctx.
func NewMessagePipe(ctx context.Context, transport Transport, name string, log logr.Logger) *MessagePipe {
	return &MessagePipe{
		transport: transport,
		ch:        concurrency.NewUnboundedChan[*MessageEnvelope](ctx),
		log:       log,
		name:      name,
	}
}

// Send enqueues a message to be written by the pipe's writer goroutine.
// This method never blocks for an extended period (UnboundedChan buffers
// internally). It is safe for concurrent use by multiple goroutines.
func (p *MessagePipe) Send(env *MessageEnvelope) {
	p.ch.In <- env
}

// CloseInput closes the pipe's input channel, signaling that no more messages
// will be sent. The pipe's Run goroutine will finish writing any buffered
// messages and then exit. The caller must ensure no goroutine calls Send after
// CloseInput returns.
func (p *MessagePipe) CloseInput() {
	close(p.ch.In)
}

// Run runs the writer loop, reading messages from the FIFO queue, assigning
// sequence numbers, and writing them to the transport. It returns when the
// context is cancelled (which closes the UnboundedChan's Out channel) or
// when a transport write error occurs.
func (p *MessagePipe) Run(ctx context.Context) error {
	for env := range p.ch.Out {
		// Assign the next sequence number.
		originalSeq := env.Seq
		newSeq := int(p.SeqCounter.Add(1))
		env.Seq = newSeq

		// For request messages, store the mapping so the opposite direction's
		// reader can remap request_seq on responses.
		if env.Type == "request" {
			p.seqMap.Store(newSeq, originalSeq)
		}

		p.log.V(1).Info("Writing message",
			"pipe", p.name,
			"message", env.Describe(),
			"originalSeq", originalSeq,
			"assignedSeq", newSeq)

		finalizedMsg, finalizeErr := env.Finalize()
		if finalizeErr != nil {
			return fmt.Errorf("%s: failed to finalize message: %w", p.name, finalizeErr)
		}

		writeErr := p.transport.WriteMessage(finalizedMsg)
		if writeErr != nil {
			return fmt.Errorf("%s: failed to write message: %w", p.name, writeErr)
		}
	}

	return ctx.Err()
}

// RemapResponseSeq looks up the original sequence number for a response's
// request_seq field. If found, it updates env.RequestSeq to the original
// value and deletes the mapping. This should be called by the reader of
// the opposite direction before enqueueing a response to its own pipe.
func (p *MessagePipe) RemapResponseSeq(env *MessageEnvelope) {
	if !env.IsResponse() {
		return
	}
	if origSeq, found := p.seqMap.LoadAndDelete(env.RequestSeq); found {
		p.log.V(1).Info("Remapping response request_seq",
			"pipe", p.name,
			"command", env.Command,
			"virtualRequestSeq", env.RequestSeq,
			"originalRequestSeq", origSeq)
		env.RequestSeq = origSeq
	}
}
