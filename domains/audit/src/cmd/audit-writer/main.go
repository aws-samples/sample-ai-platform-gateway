// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// audit-writer of the Audit domain: consumes Audit_Event from the queue and
// persists it in the append-only trail.
//
// This is a SHELL: the decision (redaction, retention) comes from the pure domain
// and persistence comes from the port. Here there is only parsing, dependency
// resolution and logging.
//
// The path is asynchronous on purpose (Req 8): the customer's administrative action
// has already responded by the time this function runs, so nothing here can
// influence that response. In exchange, nothing here may LOSE an event silently —
// a failure goes back to the queue and ends up in the DLQ, which the Backoffice
// monitors.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/aiplat/audit/internal/adapters/ddbtrail"
	"github.com/aiplat/audit/internal/auditcore"
	"github.com/aiplat/audit/internal/ports"
)

var trail ports.TrailStore

func initAWS() {
	cfg, err := awscfg.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	ddb := dynamodb.NewFromConfig(cfg)
	trail = ddbtrail.New(ddb, os.Getenv("TRAIL_TABLE"))
}

// envelope is the shape EventBridge delivers to SQS: the audit event arrives in
// `detail`.
type envelope struct {
	DetailType string          `json:"detail-type"`
	Source     string          `json:"source"`
	Detail     auditcore.Event `json:"detail"`
	Raw        json.RawMessage `json:"-"`
}

func handle(ctx context.Context, in events.SQSEvent) error {
	for _, msg := range in.Records {
		if err := one(ctx, msg.Body); err != nil {
			// Returning an error sends the message back to the queue and, once the
			// retries are exhausted, on to the DLQ. That is the desired behaviour:
			// better one item in the DLQ (visible) than a dropped event (invisible).
			return err
		}
	}
	return nil
}

func one(ctx context.Context, body string) error {
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		// An unreadable message does not improve with a retry — log it and absorb it,
		// otherwise it blocks the queue forever. The log is the trace that something
		// arrived malformed.
		logJSON(map[string]any{"event": "audit_ingest_unparseable", "error": err.Error()})
		return nil
	}
	ev := env.Detail
	if ev.Org == "" || ev.Action == "" || ev.TS == "" || ev.EventID == "" {
		logJSON(map[string]any{"event": "audit_ingest_incomplete",
			"org": ev.Org, "action": ev.Action, "has_ts": ev.TS != "", "has_id": ev.EventID != ""})
		return nil
	}

	// Category: trust the emitter's when recognized; derive it when absent. An action
	// outside the catalog is NOT dropped (Req 2.8): during a partial deploy the
	// emitter may be newer than the writer, and losing audit data is irreversible
	// while an unknown label is cosmetic.
	if cat, ok := auditcore.CategoryOf(ev.Action); ok {
		ev.Category = cat
	} else {
		logJSON(map[string]any{"event": "audit_unknown_action", "action": ev.Action, "org": ev.Org})
		if ev.Category == "" {
			ev.Category = "other"
		}
	}

	// Redaction AT INGESTION, again. The emitter already redacted (that is where it
	// belongs, the value never even travels through the queue), but a buggy emitter
	// would turn a secret into an append-only record — which by definition cannot be
	// corrected later.
	ev.Changes = auditcore.Redact(ev.Changes)
	if ev.ChangeCount == 0 {
		ev.ChangeCount = len(ev.Changes)
	}
	if chs, cut := auditcore.Truncate(ev.Changes, auditcore.MaxChanges); cut {
		ev.Changes, ev.Truncated = chs, true
	}
	if ev.Actor.Type == "" {
		ev.Actor.Type = auditcore.ActorTypeFor(ev.Actor.Role)
	}

	expires := expiresAt(ev.TS, auditcore.RetentionDays)

	if err := trail.Append(ctx, ev, expires); err != nil {
		logJSON(map[string]any{"event": "audit_append_failed", "org": ev.Org,
			"action": ev.Action, "error": err.Error()})
		return err
	}
	logJSON(map[string]any{"event": "audit_appended", "org": ev.Org, "action": ev.Action,
		"category": ev.Category, "actor": ev.Actor.Email, "changes": ev.ChangeCount})
	return nil
}

// expiresAt computes the expiration from the instant OF THE EVENT, not the instant
// of ingestion: an event that sat in the DLQ and was reprocessed days later must not
// gain extra retention, otherwise the plan's window would stop applying.
func expiresAt(ts string, days int) int64 {
	base, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		base = time.Now().UTC()
	}
	return base.AddDate(0, 0, days).Unix()
}

func logJSON(m map[string]any) {
	b, _ := json.Marshal(m)
	log.Println(string(b))
}

func main() {
	initAWS()
	lambda.Start(handle)
}
