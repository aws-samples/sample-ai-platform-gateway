// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Lambda Runtime API loop with RESPONSE STREAMING.
//
// WHY THIS FILE EXISTS AT ALL: aws-lambda-go does not implement the streaming side of the
// Runtime API. Its runtimeAPIClient.post sets only User-Agent, Content-Type and the X-Ray
// header — never `Lambda-Runtime-Function-Response-Mode: streaming` — and
// events.APIGatewayProxyStreamingResponse produces the right BYTES (metadata JSON, the
// 8-null-byte delimiter, then the payload) without anything telling Lambda to treat them
// as a stream. Checked by reading the source through v1.55.0. So returning the streaming
// response type from lambda.Start is not enough: the header has to be sent, which means
// owning the loop.
//
// MEASURED, end to end through the deployed Regional REST API:
//
//	stream=true  max_tokens=400  -> 200, 11135 bytes, 61 SSE frames
//	stream=true  max_tokens=4000 -> 200, 158895 bytes, 848 SSE frames, 56.1 s total
//	stream=false                 -> 200, one complete JSON body
//
// That second line is the point of the whole exercise: the same request under a BUFFERED
// integration returned `504 {"message":"Endpoint request timed out"}` at 29.6 s.
//
// A DEAD END WORTH RECORDING, because it cost two outages and it will look like this file
// is broken if it happens again. The first deployment of this loop returned the correct
// status code with a ZERO-BYTE body, and the CloudWatch logs were completely clean —
// START, the routing decision, END, REPORT. Nothing in the function was wrong. The cause
// was in the Terraform: aws_api_gateway_deployment.router hashed only resource IDs in its
// triggers, and an ID does not change when an attribute does, so changing the integration
// to STREAM never produced a new deployment. API Gateway kept serving the BUFFERED
// deployment against a streaming Lambda. That combination does not raise an error — it
// answers with the right status code and no body. The triggers now hash uri,
// response_transfer_mode and timeout_milliseconds, which is what makes this reproducible.
//
// The lesson generalises past streaming: when an API Gateway change appears to have no
// effect, check that the STAGE was redeployed before suspecting the integration or the
// function.
//
// This loop is deliberately minimal — one handler shape, no RPC mode, no extensions, no
// lambdacontext — so that it stays cheap to read, and cheap to delete if aws-lambda-go
// ever ships the header itself.
//
// AIPLAT_RESPONSE_MODE and the integration's transfer mode come from the SAME Terraform
// flag (var.response_streaming) so they cannot disagree. Keep it that way: a streaming
// runtime behind a buffered integration, or the reverse, breaks every request quietly.
package awslambda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/core/internal/httpapi"
)

// Runtime API constants. The version prefix is the Runtime API's own, unrelated to the
// 2021-11-15 in the API Gateway integration URI.
const (
	runtimeAPIVersion = "/2018-06-01/runtime/invocation/"

	// headerResponseMode is THE header this whole file exists to send.
	headerResponseMode = "Lambda-Runtime-Function-Response-Mode"
	responseModeStream = "streaming"

	headerRequestID  = "Lambda-Runtime-Aws-Request-Id"
	headerDeadlineMS = "Lambda-Runtime-Deadline-Ms"
	headerErrorType  = "Lambda-Runtime-Function-Error-Type"
)

// StreamingEnabled reports whether this process should run the streaming loop.
//
// A single env var rather than a build tag: the same artifact has to be deployable in
// both modes, because the safe rollout is to ship the code with the flag off and flip it
// together with the integration in one apply.
func StreamingEnabled() bool {
	return os.Getenv("AIPLAT_RESPONSE_MODE") == responseModeStream
}

// StartStreaming runs the invocation loop until the process is killed. It only returns on
// an unrecoverable error — the same contract as lambda.Start.
func StartStreaming(h httpapi.HandlerFunc) error {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		return fmt.Errorf("AWS_LAMBDA_RUNTIME_API is not set (not running inside Lambda?)")
	}
	base := "http://" + api + runtimeAPIVersion

	// No client timeout. /next is a long poll that blocks until an invocation arrives,
	// and the response POST lasts as long as the generation does — a client timeout here
	// would cut the stream mid-answer. Per-invocation time is bounded by the deadline the
	// Runtime API reports, which is applied to the context below.
	client := &http.Client{}

	for {
		evt, reqID, deadline, err := next(client, base)
		if err != nil {
			// A failure talking to the Runtime API is unrecoverable: without /next there
			// is no work to do and retrying in a tight loop would spin.
			return fmt.Errorf("runtime API next: %w", err)
		}

		if err := invokeOnce(client, base, h, evt, reqID, deadline); err != nil {
			// The error was already reported to the Runtime API by invokeOnce; the loop
			// continues because Lambda reuses the environment after a failed invocation.
			continue
		}
	}
}

// next long-polls for the following invocation.
func next(client *http.Client, base string) (payload []byte, reqID string, deadline time.Time, err error) {
	req, err := http.NewRequest(http.MethodGet, base+"next", nil)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	defer resp.Body.Close()

	payload, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	reqID = resp.Header.Get(headerRequestID)
	if ms, e := strconv.ParseInt(resp.Header.Get(headerDeadlineMS), 10, 64); e == nil {
		deadline = time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
	}
	return payload, reqID, deadline, nil
}

// invokeOnce runs the handler for one invocation and streams the response back.
func invokeOnce(client *http.Client, base string, h httpapi.HandlerFunc, payload []byte, reqID string, deadline time.Time) error {
	ctx := context.Background()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		// Cancel only AFTER the response has been fully written: the producer keeps using
		// this context while streaming (provider calls, DynamoDB, SQS). Cancelling at
		// handler return — the obvious mistake — would kill the generation as soon as the
		// first byte was ready.
		defer cancel()
	}

	// A panic must not take the process down: report it as an invocation error so Lambda
	// records the failure and the environment can serve the next request.
	var out *events.APIGatewayProxyStreamingResponse
	if err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		var e events.APIGatewayProxyRequest
		if uerr := json.Unmarshal(payload, &e); uerr != nil {
			return fmt.Errorf("unmarshal event: %w", uerr)
		}
		req, rerr := ToRequest(e)
		if rerr != nil {
			out = FromResponseStream(httpapi.Response{StatusCode: 400, Body: "bad request"})
			return nil
		}
		resp, herr := h(ctx, req)
		if herr != nil {
			return herr
		}
		out = FromResponseStream(resp)
		return nil
	}(); err != nil {
		reportError(client, base, reqID, err)
		return err
	}

	return postStream(client, base, reqID, out)
}

// postStream sends the response with the streaming response mode and chunked encoding.
//
// ContentLength = -1 is what makes net/http use chunked transfer encoding: without it Go
// would try to buffer the body to compute a length, which would defeat the entire point
// and reintroduce exactly the behaviour this file exists to remove.
func postStream(client *http.Client, base, reqID string, out *events.APIGatewayProxyStreamingResponse) error {
	defer out.Close()

	req, err := http.NewRequest(http.MethodPost, base+reqID+"/response", out)
	if err != nil {
		return err
	}
	req.ContentLength = -1
	req.Header.Set(headerResponseMode, responseModeStream)
	req.Header.Set("Content-Type", out.ContentType())
	req.Header.Set("Transfer-Encoding", "chunked")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime API response POST: status %d", resp.StatusCode)
	}
	return nil
}

// reportError tells the Runtime API the invocation failed.
func reportError(client *http.Client, base, reqID string, cause error) {
	body, _ := json.Marshal(map[string]string{
		"errorType":    "HandlerError",
		"errorMessage": cause.Error(),
	})
	req, err := http.NewRequest(http.MethodPost, base+reqID+"/error", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerErrorType, "HandlerError")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
