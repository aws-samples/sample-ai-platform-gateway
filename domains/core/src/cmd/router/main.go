// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Router (gateway-core) of the Core domain: dependency wiring and wrapper choice.
//
// This file is the ONLY part of the Core that knows which concrete adapters exist
// and how they are constructed. The request orchestration lives in
// internal/gateway; the pure decision in internal/routing. That is the thin shell
// R1.4 of hexagonal-refactor asks for: parse nothing, decide nothing — construct,
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
		Config:      ddbconfig.New(ddb, os.Getenv("CONFIG_TABLE"), deploymentOrg),
		Org:         deploymentOrg,
		Cache:       cacheImpl,
		Sem:         cacheImpl,
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
	lambda.Start(awslambda.Adapt(gateway.Handle))
}
