// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package bedrockembed implements ports.Embedder on top of Amazon Bedrock, using
// Titan Text Embeddings V2 (InvokeModel). It is the semantic cache's embedding
// generator — "our way": managed (Bedrock, no local/ONNX model), in the platform
// account, with the SMALLEST useful dimension (256) to keep storage cheap.
//
// The router's IAM already allows bedrock:InvokeModel; there is no new credential.
// A network/service failure comes back as an error so the caller can degrade
// (skipping the semantic cache).
package bedrockembed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/aiplat/core/internal/ports"
)

// Adapter calls Titan V2 through a Bedrock client (platform account).
type Adapter struct {
	Client  *bedrockruntime.Client
	ModelID string // e.g. amazon.titan-embed-text-v2:0
	Dim     int    // 256 | 512 | 1024 (defaults to 256 when 0)
}

var _ ports.Embedder = (*Adapter)(nil) // compile-time assertion

// New builds the adapter. An empty modelID falls back to Titan v2; an empty dim falls back to 256.
func New(cli *bedrockruntime.Client, modelID string, dim int) *Adapter {
	if modelID == "" {
		modelID = "amazon.titan-embed-text-v2:0"
	}
	if dim == 0 {
		dim = 256
	}
	return &Adapter{Client: cli, ModelID: modelID, Dim: dim}
}

// titanReq is the body of Titan v2's InvokeModel. normalize=true turns the cosine
// into an inner product and keeps the comparison stable across requests.
type titanReq struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanResp struct {
	Embedding []float32 `json:"embedding"`
}

// Embed returns the embedding of the text. Empty text returns an error (there is
// no point vectorizing nothing — the caller skips the semantic cache).
func (a *Adapter) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("embed: empty text")
	}
	body, _ := json.Marshal(titanReq{InputText: text, Dimensions: a.Dim, Normalize: true})
	out, err := a.Client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(a.ModelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, err
	}
	var r titanResp
	if err := json.Unmarshal(out.Body, &r); err != nil || len(r.Embedding) == 0 {
		return nil, fmt.Errorf("embed: invalid Titan response")
	}
	return r.Embedding, nil
}
