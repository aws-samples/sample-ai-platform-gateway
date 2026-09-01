// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package awslambda is the ONLY place in the Core that knows about the API
// Gateway event type. Pure protocol translation — no business decision here
// (Requirement 1.4).
//
// Feature: hexagonal-refactor, Requirements 1.2, 1.3.
package awslambda

import (
	"context"
	"encoding/base64"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aiplat/core/internal/httpapi"
)

// Adapt wraps an httpapi.HandlerFunc into the shape lambda.Start expects.
//
// REST API event (APIGatewayProxyRequest/Response), not the HTTP API's v2 shape —
// the router moved here for AWS WAF, resource policies and private-endpoint support,
// none of which HTTP API offers. Auth for this Lambda stays exactly as it was: an
// API key checked inside the domain (authResolve in internal/gateway), never
// Cognito — REST API's COGNITO_USER_POOLS authorizer is irrelevant here and is not
// attached to this route.
func Adapt(h httpapi.HandlerFunc) func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return func(ctx context.Context, e events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		req, err := ToRequest(e)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: 400, Body: "bad request"}, nil
		}
		resp, err := h(ctx, req)
		if err != nil {
			return events.APIGatewayProxyResponse{}, err
		}
		return FromResponse(resp), nil
	}
}

// ToRequest translates the API Gateway event into the Core's neutral boundary.
//
// Preserves: method, path, headers and body (decoding base64 when API Gateway
// signals IsBase64Encoded — this decoding used to live inside handle(); moving it
// here is what frees the domain from knowing base64 sits along the way).
func ToRequest(e events.APIGatewayProxyRequest) (httpapi.Request, error) {
	raw := e.Body
	if e.IsBase64Encoded {
		if dec, derr := base64.StdEncoding.DecodeString(raw); derr == nil {
			raw = string(dec)
		}
	}
	path := e.Path
	if path == "" {
		path = e.Resource
	}
	return httpapi.Request{
		Method:    e.HTTPMethod,
		Path:      path,
		Headers:   e.Headers,
		Body:      raw,
		RequestID: e.RequestContext.RequestID,
	}, nil
}

// FromResponse translates the neutral response back into the API Gateway format.
func FromResponse(r httpapi.Response) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: r.StatusCode,
		Headers:    r.Headers,
		Body:       r.Body,
	}
}
