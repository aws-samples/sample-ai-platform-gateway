// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package bedrock is the outbound adapter for Amazon Bedrock (Converse API).
// It implements ports.Provider.
//
// (callBedrock, convertToolsToBedrockConfig, toSmithyDocument and bedrockFor)
// without rewriting the logic. Client resolution (pooled account vs BYO
// cross-account role, with a client cache) now lives in the Pool; the Adapter binds
// a model + route + pool and translates ports.Message into Converse blocks.
//
// Prompt caching: with SDK v1.57.2 the counters (CacheReadInputTokens/
// CacheWriteInputTokens) are read in a typed way and the adapter inserts a cache
// point at the end of system when the route enables CachePrefix. The old
// reflection-based read (bedrockCacheTokens) was removed along with the old pin.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aiplat/core/internal/ports"
)

// Route carries the per-route Bedrock connection parameters (BYO cross-account).
type Route struct {
	RoleARN    string
	ExternalID string
	Region     string
}

type brEntry struct {
	cli *bedrockruntime.Client
	at  time.Time
}

// Pool resolves and caches Bedrock clients (platform account or assumed role).
type Pool struct {
	base aws.Config
	def  *bedrockruntime.Client
	sts  *sts.Client

	mu    sync.Mutex
	cache map[string]brEntry
}

// NewPool builds the pool with the base config, the default client (the Lambda's
// region) and the STS client used to assume a role in the customer's account.
func NewPool(base aws.Config, def *bedrockruntime.Client, stsc *sts.Client) *Pool {
	return &Pool{base: base, def: def, sts: stsc, cache: map[string]brEntry{}}
}

// ClientFor returns the right Bedrock client (bedrockFor's logic, verbatim):
//   - without role_arn → platform account; honors the route's `region`
//   - with role_arn → assumes the role in the CUSTOMER's account (BYO, spend/data there)
//
// Temporary credentials are cached per role for up to ~50 min.
func (p *Pool) ClientFor(ctx context.Context, r Route) *bedrockruntime.Client {
	if r.RoleARN == "" {
		if r.Region == "" || r.Region == p.base.Region {
			return p.def
		}
		key := "|" + r.Region
		p.mu.Lock()
		if e, ok := p.cache[key]; ok {
			p.mu.Unlock()
			return e.cli
		}
		p.mu.Unlock()
		cfg := p.base.Copy()
		cfg.Region = r.Region
		cli := bedrockruntime.NewFromConfig(cfg)
		p.mu.Lock()
		p.cache[key] = brEntry{cli: cli, at: time.Now()}
		p.mu.Unlock()
		return cli
	}
	key := r.RoleARN + "|" + r.Region
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && time.Since(e.at) < 50*time.Minute {
		p.mu.Unlock()
		return e.cli
	}
	p.mu.Unlock()

	prov := stscreds.NewAssumeRoleProvider(p.sts, r.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "aiplat-gateway"
		if r.ExternalID != "" {
			o.ExternalID = &r.ExternalID
		}
	})
	cfg := p.base.Copy()
	cfg.Credentials = aws.NewCredentialsCache(prov)
	if r.Region != "" {
		cfg.Region = r.Region
	}
	cli := bedrockruntime.NewFromConfig(cfg)

	p.mu.Lock()
	p.cache[key] = brEntry{cli: cli, at: time.Now()}
	p.mu.Unlock()
	return cli
}

// Adapter binds a model + route + pool. It implements ports.Provider.
type Adapter struct {
	Pool    *Pool
	ModelID string
	Route   Route

	// CachePrefix turns on the provider's prompt caching: when true, the adapter
	// marks the end of the system block with a cache point (Converse), telling
	// Bedrock to cache the prompt's stable prefix. It is a per-route opt-in
	// (config.routing[model].prompt_cache) because the cache-write charges a premium
	// and only pays off when the prefix does repeat across requests.
	CachePrefix bool
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	cli := a.Pool.ClientFor(ctx, a.Route)
	return callBedrock(ctx, cli, a.ModelID, in, a.CachePrefix)
}

// converseAPI is the narrow slice of the Bedrock client the buffered path needs, so the
// request it BUILDS can be inspected without a live client.
//
// The streaming path already had this seam (converseStreamAPI) and the buffered one did not,
// which is not a cosmetic asymmetry: it meant no test could see what callBedrock actually put
// on the wire. Reintroducing "drop every inference parameter here" as an experiment left the
// suite green, because the only coverage was of the helper and never of the call site. Same
// structural gap that let the reasoning block go unhandled in this file for as long as it did.
type converseAPI interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// applyInference fills Converse's InferenceConfig and AdditionalModelRequestFields from
// the request.
//
// Written once and used by BOTH the buffered and the streaming input, because the two
// structs are separate types with identical members: the previous shape (each path
// building its own literal) is exactly how the two paths drift, and the half that drifts
// silently is the one nobody tests.
//
// EVERY field is conditional. Sending a zero we invented is not a neutral act — a
// temperature of 0 makes the model deterministic and a max_tokens of 0 is rejected — so
// "the client said nothing" has to reach the provider as an absent field, letting the
// model's own default stand.
func applyInference(in ports.InvokeInput) (*btypes.InferenceConfiguration, document.Interface) {
	var cfg *btypes.InferenceConfiguration
	ensure := func() *btypes.InferenceConfiguration {
		if cfg == nil {
			cfg = &btypes.InferenceConfiguration{}
		}
		return cfg
	}
	if in.MaxOutputTokens > 0 {
		ensure().MaxTokens = aws.Int32(int32(in.MaxOutputTokens))
	}
	if t := in.Inference.Temperature; t != nil {
		ensure().Temperature = aws.Float32(float32(*t))
	}
	if p := in.Inference.TopP; p != nil {
		ensure().TopP = aws.Float32(float32(*p))
	}
	if len(in.Inference.Stop) > 0 {
		ensure().StopSequences = in.Inference.Stop
	}

	// Extended thinking is not part of Converse's typed surface: it travels in
	// additionalModelRequestFields, which is a free-form document. That is precisely why
	// the gateway resolves and clamps the budget BEFORE here — this function must never be
	// the place that decides how much thinking a customer may buy, and it must never pass
	// through a value it did not construct itself.
	r := in.Inference.Reasoning
	if r == nil {
		return cfg, nil
	}
	if r.Disabled {
		// An explicit "do not think". Bedrock has no "off" switch for a model that reasons
		// intrinsically (DeepSeek-R1 always thinks), so this only silences the models that
		// treat thinking as opt-in — which is honest: we send what the provider accepts and
		// never pretend to have disabled something we cannot.
		return cfg, nil
	}
	if r.BudgetTokens <= 0 {
		return cfg, nil
	}
	// Anthropic on Bedrock requires max_tokens > budget_tokens. The gateway guarantees the
	// invariant and records any clamp in the ledger; asserting it again here would either
	// duplicate that logic or disagree with it.
	return cfg, toSmithyDocument(map[string]interface{}{
		"thinking": map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": r.BudgetTokens,
		},
	})
}

// ConvertToolsToBedrockConfig converts tools from the boundary format into
// Bedrock's ToolConfiguration.
func ConvertToolsToBedrockConfig(tools []ports.ToolDef) *btypes.ToolConfiguration {
	if len(tools) == 0 {
		return nil
	}
	var bedrockTools []btypes.Tool
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		var inputSchema btypes.ToolInputSchema
		if t.Parameters != nil {
			inputSchema = &btypes.ToolInputSchemaMemberJson{Value: toSmithyDocument(t.Parameters)}
		}
		spec := btypes.ToolSpecification{
			Name:        aws.String(t.Name),
			InputSchema: inputSchema,
		}
		if t.Description != "" {
			spec.Description = aws.String(t.Description)
		}
		bedrockTools = append(bedrockTools, &btypes.ToolMemberToolSpec{Value: spec})
	}
	if len(bedrockTools) == 0 {
		return nil
	}
	return &btypes.ToolConfiguration{Tools: bedrockTools}
}

func toSmithyDocument(v interface{}) document.Interface {
	return document.NewLazyDocument(v)
}

// bedrockImageFormats is Converse's declared ImageFormat enum. A format outside
// this set (or missing) is dropped rather than sent — Bedrock rejects an unknown
// format for the whole request, and one bad part should not fail the other
// (text-only) content in the same message.
var bedrockImageFormats = map[string]btypes.ImageFormat{
	"png":  btypes.ImageFormatPng,
	"jpeg": btypes.ImageFormatJpeg,
	"jpg":  btypes.ImageFormatJpeg, // OpenAI-dialect clients commonly send "jpg"; Bedrock only knows "jpeg"
	"gif":  btypes.ImageFormatGif,
	"webp": btypes.ImageFormatWebp,
}

// imageBlocks converts the boundary's decoded image parts (ports.ImagePart) into
// Bedrock Converse ContentBlocks. This is what closes the gap where images were
// silently dropped: convertMessages used to build every user message from m.Text
// alone, so a multimodal request that passed the routing eligibility filter
// (hasImage + Capabilities.Multimodal) still reached the model as text-only.
func imageBlocks(images []ports.ImagePart) []btypes.ContentBlock {
	var out []btypes.ContentBlock
	for _, img := range images {
		format, ok := bedrockImageFormats[strings.ToLower(img.Format)]
		if !ok || len(img.Bytes) == 0 {
			continue
		}
		out = append(out, &btypes.ContentBlockMemberImage{
			Value: btypes.ImageBlock{
				Format: format,
				Source: &btypes.ImageSourceMemberBytes{Value: img.Bytes},
			},
		})
	}
	return out
}

// convertMessages translates the messages from the boundary format (OpenAI dialect)
// into Bedrock Converse's (system, conversation) pair.
//
// Sensitive point: in OpenAI each tool result is a separate message
// (`role:"tool"` + tool_call_id). In Bedrock, ALL the toolResults of a single turn
// must arrive GROUPED as multiple blocks inside ONE `role:"user"` message, in the
// order of the assistant's toolUse blocks. That is why CONSECUTIVE `tool` messages
// are merged into a single user message — otherwise Bedrock rejects with
// "Expected toolResult blocks ...". It is a pure function (no network SDK) so the
// grouping can have a regression test.
func convertMessages(msgs []ports.Message) (system []btypes.SystemContentBlock, conv []btypes.Message) {
	system, conv = []btypes.SystemContentBlock{}, []btypes.Message{}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case "system":
			if t := m.Text; t != "" {
				system = append(system, &btypes.SystemContentBlockMemberText{Value: t})
			}
			continue

		case "tool":
			// Merge this and every following `tool` message into a single
			// content block of ONE `user` message, preserving the order.
			var results []btypes.ContentBlock
			for ; i < len(msgs) && msgs[i].Role == "tool"; i++ {
				tm := msgs[i]
				if tm.ToolCallID == "" {
					continue
				}
				results = append(results, &btypes.ContentBlockMemberToolResult{
					Value: btypes.ToolResultBlock{
						ToolUseId: aws.String(tm.ToolCallID),
						Content: []btypes.ToolResultContentBlock{
							&btypes.ToolResultContentBlockMemberText{Value: tm.Text},
						},
					},
				})
			}
			i-- // the outer for increments; step back so the next message is not skipped
			if len(results) > 0 {
				conv = append(conv, btypes.Message{Role: btypes.ConversationRoleUser, Content: results})
			}
			continue

		case "assistant":
			if len(m.ToolCalls) > 0 {
				var blocks []btypes.ContentBlock
				if t := m.Text; t != "" {
					blocks = append(blocks, &btypes.ContentBlockMemberText{Value: t})
				}
				for _, tc := range m.ToolCalls {
					var args map[string]interface{}
					if tc.Arguments != "" {
						json.Unmarshal([]byte(tc.Arguments), &args)
					}
					if args == nil {
						args = map[string]interface{}{}
					}
					blocks = append(blocks, &btypes.ContentBlockMemberToolUse{
						Value: btypes.ToolUseBlock{
							ToolUseId: aws.String(tc.ID),
							Name:      aws.String(tc.Name),
							Input:     toSmithyDocument(args),
						},
					})
				}
				conv = append(conv, btypes.Message{Role: btypes.ConversationRoleAssistant, Content: blocks})
				continue
			}
			conv = append(conv, btypes.Message{Role: btypes.ConversationRoleAssistant,
				Content: []btypes.ContentBlock{&btypes.ContentBlockMemberText{Value: m.Text}}})
			continue
		}

		blocks := []btypes.ContentBlock{&btypes.ContentBlockMemberText{Value: m.Text}}
		blocks = append(blocks, imageBlocks(m.Images)...)
		conv = append(conv, btypes.Message{Role: btypes.ConversationRoleUser, Content: blocks})
	}
	return system, conv
}

func callBedrock(ctx context.Context, cli converseAPI, modelID string, in ports.InvokeInput, cachePrefix bool) (ports.Result, error) {
	system, conv := convertMessages(in.Messages)

	// Prompt caching (per-route opt-in): marks the end of system with a cache point.
	// Bedrock caches the stable prefix (system) and charges a cheap cache-read on
	// following requests whose prefix matches. Only meaningful when system is present.
	if cachePrefix && len(system) > 0 {
		system = append(system, &btypes.SystemContentBlockMemberCachePoint{
			Value: btypes.CachePointBlock{Type: btypes.CachePointTypeDefault},
		})
	}

	input := &bedrockruntime.ConverseInput{ModelId: &modelID, Messages: conv}
	if len(system) > 0 {
		input.System = system
	}
	if tc := ConvertToolsToBedrockConfig(in.Tools); tc != nil {
		input.ToolConfig = tc
	}
	if cfg, extra := applyInference(in); cfg != nil || extra != nil {
		input.InferenceConfig = cfg
		input.AdditionalModelRequestFields = extra
	}

	out, err := cli.Converse(ctx, input)
	if err != nil {
		return ports.Result{}, err
	}
	return parseConverseOutput(out)
}

// parseConverseOutput turns a buffered Converse response into a ports.Result.
//
// Split out of callBedrock so it can be tested at all: the parsing used to sit inline,
// one statement after a live cli.Converse call, which meant the only way to exercise it
// was to call Bedrock for real. That is exactly how the reasoning block went unhandled
// here for as long as it did — there was no seam at which to notice.
func parseConverseOutput(out *bedrockruntime.ConverseOutput) (ports.Result, error) {
	if out == nil {
		return ports.Result{}, fmt.Errorf("empty bedrock output")
	}
	msg, ok := out.Output.(*btypes.ConverseOutputMemberMessage)
	if !ok || len(msg.Value.Content) == 0 {
		return ports.Result{}, fmt.Errorf("empty bedrock output")
	}

	txt := ""
	var toolCalls []ports.ToolCall
	stopReason := ""
	if out.StopReason != "" {
		stopReason = string(out.StopReason)
	}
	reasoning, reasoningSig := "", ""
	reasoningRedacted := false

	for _, block := range msg.Value.Content {
		switch b := block.(type) {
		case *btypes.ContentBlockMemberText:
			txt = b.Value
		case *btypes.ContentBlockMemberReasoningContent:
			// Same defect as the streaming switch had: a thinking model returns its chain
			// of thought as its OWN content block, and a switch that only knows text and
			// toolUse drops it. Buffered mode hid it better than streaming did — the answer
			// still arrived, so nothing looked broken, while the signature that lets the
			// chain continue on the next turn was thrown away every time.
			switch r := b.Value.(type) {
			case *btypes.ReasoningContentBlockMemberReasoningText:
				reasoning += aws.ToString(r.Value.Text)
				// The signature is opaque metadata, never content. It is also the only
				// thing that makes this reasoning reusable in a later turn, which is why
				// it is kept even though nothing renders it.
				if sig := aws.ToString(r.Value.Signature); sig != "" {
					reasoningSig = sig
				}
			case *btypes.ReasoningContentBlockMemberRedactedContent:
				// The model thought and the provider encrypted it. Recorded so that
				// "did not think" stays distinguishable from "not shown to us".
				reasoningRedacted = true
			}
		case *btypes.ContentBlockMemberToolUse:
			var argsMap interface{}
			if b.Value.Input != nil {
				b.Value.Input.UnmarshalSmithyDocument(&argsMap)
			}
			argsJSON, _ := json.Marshal(argsMap)
			if argsJSON == nil || string(argsJSON) == "null" {
				argsJSON = []byte("{}")
			}
			toolCalls = append(toolCalls, ports.ToolCall{
				ID:        aws.ToString(b.Value.ToolUseId),
				Name:      aws.ToString(b.Value.Name),
				Arguments: string(argsJSON),
			})
		}
	}

	tin, tout := 0, 0
	cacheRead, cacheWrite := 0, 0
	cacheConv := ports.CacheCountersAbsent
	if out.Usage != nil {
		if out.Usage.InputTokens != nil {
			tin = int(*out.Usage.InputTokens)
		}
		if out.Usage.OutputTokens != nil {
			tout = int(*out.Usage.OutputTokens)
		}
		// Typed read of the prompt caching counters (SDK v1.57.2+).
		if out.Usage.CacheReadInputTokens != nil || out.Usage.CacheWriteInputTokens != nil {
			cacheRead = int(aws.ToInt32(out.Usage.CacheReadInputTokens))
			cacheWrite = int(aws.ToInt32(out.Usage.CacheWriteInputTokens))
			cacheConv = ports.CacheCountersReported
		}
	}
	return ports.Result{
		Text: txt, InputTokens: tin, OutputTokens: tout, ToolCalls: toolCalls, StopReason: stopReason,
		CacheReadInputTokens: cacheRead, CacheWriteInputTokens: cacheWrite, CacheCounters: cacheConv,
		Reasoning: reasoning, ReasoningChars: len(reasoning),
		ReasoningSignature: reasoningSig, ReasoningRedacted: reasoningRedacted,
	}, nil
}
