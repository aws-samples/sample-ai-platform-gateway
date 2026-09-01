// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// audit-archiver: exports every new audit-trail record to S3 (cold tier).
//
// Why this exists: the trail's DynamoDB table is the HOT tier — Query by org, fast,
// what the Console reads. It carries a 365-day TTL (auditcore.RetentionDays), so an
// item eventually disappears from DynamoDB. Without this exporter, "disappears from
// DynamoDB" would mean "gone forever" — unacceptable for an audit trail, whose whole
// point is to survive the thing it recorded. This Lambda makes the TTL delete safe:
// once a record has been archived to S3, DynamoDB dropping it is a storage-cost
// optimization, not data loss.
//
//	writer (SQS) --PutItem--> trail (DynamoDB) --Streams(NEW_IMAGE)--> archiver --> S3
//
// Consumes DynamoDB Streams, one object per record, partitioned by event date
// (not ingestion date — same rule as the writer's TTL calculation) so a bulk
// backfill or a delayed DLQ replay lands under the day it actually happened, not
// the day it was reprocessed.
//
// Only INSERT events are archived. The trail is append-only by construction
// (ddbtrail.Append conditions on attribute_not_exists(sk); the writer's IAM has no
// UpdateItem) — a MODIFY should never happen, and a REMOVE is the TTL sweep deleting
// an item ALREADY archived (that is the point of this Lambda), so both are no-ops
// here rather than errors.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	s3Client *s3.Client
	bucket   = os.Getenv("ARCHIVE_BUCKET")
)

// attrToInterface converts one DynamoDB Streams attribute value into a plain Go
// value, so the whole record can be marshalled as ordinary JSON in S3 — the cold
// tier is meant to be read by Athena/humans, not by a DynamoDB SDK, so it must not
// carry DynamoDB's typed-attribute envelope ({"S": "..."}) in every field.
func attrToInterface(av events.DynamoDBAttributeValue) interface{} {
	switch av.DataType() {
	case events.DataTypeString:
		return av.String()
	case events.DataTypeNumber:
		// Kept as a string: a numeric AttributeValue may be an integer (change_count)
		// or lose precision as float64 in JSON. The archive is for reading, not
		// arithmetic — ddbtrail.go already made this same trade-off for `changes`.
		n, _ := av.Float()
		return n
	case events.DataTypeBoolean:
		return av.Boolean()
	case events.DataTypeNull:
		return nil
	case events.DataTypeList:
		out := make([]interface{}, 0, len(av.List()))
		for _, v := range av.List() {
			out = append(out, attrToInterface(v))
		}
		return out
	case events.DataTypeMap:
		out := make(map[string]interface{}, len(av.Map()))
		for k, v := range av.Map() {
			out[k] = attrToInterface(v)
		}
		return out
	default:
		return nil
	}
}

// recordKey builds the S3 key from the event's own timestamp (not ingestion time),
// so a delayed/reprocessed write is filed under the day it actually happened —
// mirroring the TTL rule in the writer (expiresAt is computed from ev.TS).
// Falls back to "unknown" when ts is missing/malformed, which should not happen
// (the trail always writes ts) but must not crash the batch if it somehow does.
func recordKey(org, eventID, ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return fmt.Sprintf("unknown/%s/%s.json", org, eventID)
	}
	return fmt.Sprintf("%04d/%02d/%02d/%s/%s.json", t.Year(), t.Month(), t.Day(), org, eventID)
}

// orgOf strips the "AUDIT#" prefix from pk — kept local (not imported from
// auditcore) so this Lambda's IAM/dependency footprint stays exactly what it needs:
// read the stream, write S3. Pulling in the pure domain for one string trim would
// add a dependency for no behavior gained.
func orgOf(pk string) string {
	return strings.TrimPrefix(pk, "AUDIT#")
}

func archiveOne(ctx context.Context, rec events.DynamoDBEventRecord) error {
	// TTL deletes and (never-expected) modifies are not archived: a REMOVE means the
	// item already lived its full hot life and — per the reason this Lambda exists —
	// was archived back when it was INSERTed. Re-archiving on delete would just
	// duplicate the same object under a second write path for no benefit.
	if rec.EventName != "INSERT" {
		return nil
	}

	img := rec.Change.NewImage
	if img == nil {
		return nil
	}
	pk := img["pk"].String()
	eventID := img["event_id"].String()
	ts := img["ts"].String()
	if pk == "" || eventID == "" {
		// Missing key fields would produce an unfindable/ambiguous S3 object — skip
		// rather than write a record nobody can locate by org/date/event_id.
		return nil
	}

	plain := make(map[string]interface{}, len(img))
	for k, v := range img {
		plain[k] = attrToInterface(v)
	}
	body, err := json.Marshal(plain)
	if err != nil {
		return fmt.Errorf("marshal event_id=%s: %w", eventID, err)
	}

	key := recordKey(orgOf(pk), eventID, ts)
	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("put s3 key=%s: %w", key, err)
	}
	return nil
}

func handle(ctx context.Context, e events.DynamoDBEvent) error {
	if bucket == "" {
		return fmt.Errorf("missing env ARCHIVE_BUCKET")
	}
	for _, rec := range e.Records {
		if err := archiveOne(ctx, rec); err != nil {
			// A real error goes back to the stream batch (retried, then to the
			// mapping's on-failure destination if configured) — losing an archive
			// write silently would defeat the whole point of this Lambda.
			return err
		}
	}
	return nil
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	s3Client = s3.NewFromConfig(cfg)
	lambda.Start(handle)
}
