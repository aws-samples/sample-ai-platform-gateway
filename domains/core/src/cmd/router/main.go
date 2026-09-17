// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Router (gateway-core) of the Core domain: dependency wiring and wrapper choice.
//
// This file is the ONLY part of the Core that knows which concrete adapters exist
// and how they are constructed. The request orchestration lives in
// internal/gateway; the pure decision in internal/routing. That is the thin shell
// The wiring rule is: parse nothing, decide nothing — construct,
// wire, choose the wrapper.
//
// AIPLAT_SERVE_ADDR: when set, the binary listens on local HTTP instead of starting
// the Lambda runtime (Requirement 1.5, 11.1). It is what allows running the full
// request path without SAM and without a deploy (Requirement 11.2), and the same
// path a future Fargate/App Runner deployment would use.
//
// Local use: AIPLAT_SERVE_ADDR=:8080 CONFIG_TABLE=... ... go run ./cmd/router
//
// Server mode reuses the SAME API key authentication as Lambda mode (authResolve,
// inside the gateway shell) — it is never exposed without auth (Requirement 11.4).
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aiplat/core/internal/adapters/bedrock"
	"github.com/aiplat/core/internal/adapters/bedrockembed"
	"github.com/aiplat/core/internal/adapters/bedrockgateway"
	"github.com/aiplat/core/internal/adapters/ddbcache"
	"github.com/aiplat/core/internal/adapters/ddbconfig"
	"github.com/aiplat/core/internal/adapters/ddbhints"
	"github.com/aiplat/core/internal/adapters/ddbkeys"
	"github.com/aiplat/core/internal/adapters/ddblimits"
	"github.com/aiplat/core/internal/adapters/secrets"
	"github.com/aiplat/core/internal/adapters/sqsusage"
	"github.com/aiplat/core/internal/awslambda"
	"github.com/aiplat/core/internal/gateway"
	"github.com/aiplat/core/internal/httpapi"
)

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddb := dynamodb.NewFromConfig(cfg)
	// The cache adapter serves two roles: the response cache (ports.Cache) and the
	// semantic index store (same table; the index lives under SEMIDX#<org>).
	cacheImpl := ddbcache.New(ddb, os.Getenv("CACHE_TABLE"))
	// HINTS_TABLE is optional: absent leaves Hints nil and the decision falls back
	// to the heuristic — the contract's normal degraded path.
	var hints *ddbhints.Reader
	if hintsTbl := os.Getenv("HINTS_TABLE"); hintsTbl != "" {
		hints = ddbhints.New(ddb, hintsTbl)
	}
	// Mirrors ddbconfig.New's own fallback: a missing DEPLOYMENT_ORG degrades to a
	// stable (if wrong) value instead of producing "aiplat-" with nothing after it.
	deploymentOrg := os.Getenv("DEPLOYMENT_ORG")
	if deploymentOrg == "" {
		deploymentOrg = "default"
	}
	gateway.Wire(gateway.Deps{
		BedrockPool: bedrock.NewPool(cfg, bedrockruntime.NewFromConfig(cfg), sts.NewFromConfig(cfg)),
		// AgentCore Gateway calls are signed with the deployment's own credentials
		// (service bedrock-agentcore); the gateway's execution role is what reaches the
		// model. Built unconditionally because it is just a signer over cfg — a route has
		// to name provider "bedrock_gateway" for any of it to be used.
		GatewaySigner: bedrockgateway.NewSigV4(cfg),
		Config:        ddbconfig.New(ddb, os.Getenv("CONFIG_TABLE"), deploymentOrg),
		Org:           deploymentOrg,
		Cache:         cacheImpl,
		Sem:           cacheImpl,
		// Semantic cache embedder: Titan v2 in the platform account. An empty
		// EMBED_MODEL falls back to the adapter's default; no new mandatory env var.
		Embedder: bedrockembed.New(bedrockruntime.NewFromConfig(cfg), os.Getenv("EMBED_MODEL"), 256),
		Limits:   ddblimits.New(ddb, os.Getenv("LIMITS_TABLE")),
		Usage:    sqsusage.New(sqs.NewFromConfig(cfg), os.Getenv("USAGE_QUEUE_URL")),
		Secrets:  secrets.New(secretsmanager.NewFromConfig(cfg)),
		Keys:     ddbkeys.New(ddb, os.Getenv("API_KEYS_TABLE")),
		Hints:    hints,
	})
	if addr := os.Getenv("AIPLAT_SERVE_ADDR"); addr != "" {
		if err := http.ListenAndServe(addr, httpapi.New(gateway.Handle)); err != nil {
			panic(err)
		}
		return
	}
	// Two response transports, selected by AIPLAT_RESPONSE_MODE.
	//
	// STREAMING runs our own Runtime API loop (internal/awslambda/runtimeapi.go) because
	// aws-lambda-go never sends `Lambda-Runtime-Function-Response-Mode: streaming`
	// (checked by reading the source through v1.55.0), so returning the streaming response
	// type from lambda.Start is not sufficient. It is measured working through the deployed
	// REST API, and it is what removes the API Gateway 504 ceiling: the same 4000-token
	// generation that timed out at 29.6 s buffered now completes in 56 s over 848 SSE
	// frames. Details and the false lead that cost two outages are in runtimeapi.go.
	//
	// BUFFERED keeps lambda.Start and the SDK, and stays the fallback: one variable turns
	// it back on without a rebuild, which is how those outages were recovered.
	//
	// The env var and the API Gateway integration's transfer mode come from the SAME
	// Terraform flag (var.response_streaming) precisely so they cannot disagree. A
	// streaming runtime behind a buffered integration — or the reverse — breaks every
	// request, and it breaks it quietly: the right status code with an empty body.
	if awslambda.StreamingEnabled() {
		if err := awslambda.StartStreaming(gateway.Handle); err != nil {
			panic(err)
		}
		return
	}
	lambda.Start(awslambda.Adapt(gateway.Handle))
}
