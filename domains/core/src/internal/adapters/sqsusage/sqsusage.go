// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package sqsusage is the outbound adapter that sends the Usage_Record to the
// Observability SQS queue.
//
// (emitUsage) without rewriting the logic. It preserves the ASYNCHRONOUS/
// best-effort nature: the emission is fire-and-forget and the response to the
// client never waits for it; an empty queue (queue URL not configured) is a
// silent no-op.
package sqsusage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/aiplat/core/internal/ports"
)

// Sink publishes the Usage_Record to the queue. It implements the Core's usage emission.
type Sink struct {
	sqs      *sqs.Client
	queueURL string
}

var _ ports.UsageSink = (*Sink)(nil) // compile-time assertion

// New builds the adapter with the SQS client and the queue URL (may be "").
func New(client *sqs.Client, queueURL string) *Sink {
	return &Sink{sqs: client, queueURL: queueURL}
}

// Emit sends the record. Best-effort: with no queue configured it does nothing;
// the send error is ignored on purpose (the response path does not depend on this
// — a lost event goes to retry/DLQ through SQS's own upstream semantics).
func (s *Sink) Emit(ctx context.Context, rec map[string]interface{}) {
	if s.queueURL == "" {
		return
	}
	b, _ := json.Marshal(rec)
	body := string(b)
	if _, err := s.sqs.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &s.queueURL, MessageBody: &body}); err != nil {
		// The response path does not depend on this send, but a dropped usage event must
		// not vanish silently (CORE-NFR-002): a message over the 256 KB limit, or a send
		// on an already-cancelled request context, used to be lost with no trace while the
		// request still returned 200 and the provider still billed. Logging the whole
		// record to stdout → CloudWatch makes it recoverable from Logs Insights. A bare
		// token (no interpolation) lets a metric filter count the drops.
		fmt.Printf(`{"lvl":"error","evt":"usage_emit_failed","error":%q,"record":%s}`+"\n", err.Error(), body)
		fmt.Println("AIPLAT_USAGE_EMIT_FAILED")
	}
}
