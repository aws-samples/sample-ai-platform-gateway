// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package anthropic is the outbound adapter for Anthropic's native API
// (Messages API), its own dialect. It implements ports.Provider.
//
// rewriting the logic. Non-system messages go in the body with the same shape
// chatMsg used to marshal (content raw/null/array, name, tool_calls, tool_call_id),
// rebuilt from ports.Message; system messages are concatenated into the dedicated
// `system` field, as in the original.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// Adapter is a connection bound to an Anthropic model.
type Adapter struct {
	HTTP    *http.Client
	BaseURL string
	ModelID string
	APIKey  string

	// CachePrefix turns on the provider's prompt caching: when true, the adapter
	// sends `system` in the block format with `cache_control: ephemeral` at the end,
	// telling Anthropic to cache the stable prefix (system). It is the same
	// per-route opt-in as Bedrock's (config.routing[model].prompt_cache) — the
	// cache-write charges a premium and only pays off when the prefix repeats.
	CachePrefix bool

	// Transport overrides the HTTP details for a host that speaks this SAME dialect
	// behind a different door. Zero value = Anthropic's own API, unchanged.
	Transport Transport
}

// SignFunc signs an outbound request in place. payload is the exact body being sent,
// which SigV4 needs to hash — reading it back off req.Body would consume it.
type SignFunc func(ctx context.Context, req *http.Request, payload []byte) error

// Transport is the set of HTTP-level differences between hosts that speak the Messages
// API. It exists so that a second such host reuses this package's TRANSLATION rather
// than copying it.
//
// The translation is the expensive, subtle part: the tool-result grouping rule, the
// per-type content-block dispatch, the two-event streaming usage accounting, the
// cache_control placement. Amazon Bedrock AgentCore Gateway speaks exactly that dialect
// and differs only in where the request goes and how it is authenticated. Duplicating
// ~600 lines to change a URL and an auth header would guarantee the two copies drift,
// and the drift would be silent: a request that is merely IGNORED by the provider still
// returns a plausible answer, which is precisely the class of bug this package's own
// history records (the OpenAI dialect was being sent here and quietly discarded).
type Transport struct {
	// Path is the endpoint path appended to BaseURL. Empty = "/v1/messages".
	Path string
	// ReasoningStyle selects which extended-thinking request shape this MODEL accepts.
	// Empty = ReasoningStyleBudget. See the constants for why this cannot be inferred.
	ReasoningStyle string
	// BodyVersion, when non-empty, sends `anthropic_version` in the BODY and omits the
	// `anthropic-version` HEADER. Anthropic's own API takes the header; the Bedrock
	// family requires the field in the body and rejects a request without it
	// ("anthropic_version: Field required"). They are mutually exclusive, not additive.
	BodyVersion string
	// Sign replaces API-key auth. Non-nil means the request is signed and no `x-api-key`
	// header is set; a host reached this way has no API key to send.
	Sign SignFunc
	// Label names the provider in error messages. Empty = "anthropic". A gateway route
	// failing with "anthropic 403" would send whoever reads the log to the wrong service.
	Label string
}

// Extended-thinking request shapes. They are MUTUALLY EXCLUSIVE per model, which is why
// this is a declaration and not a default: sending the wrong one is a hard 400, so neither
// can be used blindly.
//
// Measured through Amazon Bedrock AgentCore Gateway on 2026-09-16:
//
//	model          thinking.enabled+budget          thinking.adaptive+effort
//	haiku-4-5      thinking block, 139 tokens       400 "adaptive thinking is not supported"
//	sonnet-5       400 "not supported for this      thinking block, 27 tokens
//	               model. Use thinking.type.
//	               adaptive and output_config.
//	               effort"
//	opus-4-8       400 (same)                       thinking block, 45 tokens
//
// Nothing in the model id makes the split derivable — it tracks a model-family API change,
// not a naming rule — so guessing from the string would break on the next model either way.
const (
	// ReasoningStyleBudget sends thinking:{type:"enabled",budget_tokens:N}. The original
	// shape, and what Anthropic's own API and the older Bedrock models take.
	ReasoningStyleBudget = "budget"
	// ReasoningStyleAdaptive sends thinking:{type:"adaptive"} plus, when the client named
	// one, output_config:{effort:...}. The model decides how much to think.
	ReasoningStyleAdaptive = "adaptive"
)

// path returns the endpoint path for this transport.
func (t Transport) path() string {
	if t.Path == "" {
		return "/v1/messages"
	}
	return t.Path
}

// label returns the provider name used in error messages.
func (t Transport) label() string {
	if t.Label == "" {
		return "anthropic"
	}
	return t.Label
}

var _ ports.Provider = (*Adapter)(nil) // compile-time assertion

// wireMsg is ONE message in Anthropic's Messages API format.
//
// It used to be a copy of the OpenAI dialect's shape — `tool_calls`, `tool_call_id`, `name`,
// and `content` passed through verbatim from the client. None of those field names exist in
// this API, so the effect was not an error: Anthropic ignored what it did not recognise and
// answered as if the request had been text-only. Tool use never reached the model, tool
// results never reached it either, and a multimodal message arrived as an OpenAI
// `image_url` part that Anthropic does not understand.
type wireMsg struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock is one content block. Anthropic uses a tagged union, so a single struct with
// omitempty covers every variant without needing a marshaller per type.
type wireBlock struct {
	Type string `json:"type"`
	// type=text
	Text string `json:"text,omitempty"`
	// type=image
	Source *wireImageSource `json:"source,omitempty"`
	// type=tool_use
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`
	// type=tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content of a tool_result is a plain string in the simple form this gateway emits.
	// Declared as interface{} so the field is omitted entirely on the other block types.
	ResultContent interface{} `json:"content,omitempty"`
}

type wireImageSource struct {
	Type      string `json:"type"`       // always "base64" here
	MediaType string `json:"media_type"` // "image/png", …
	Data      string `json:"data"`       // base64
}

// wireTool is a tool declaration. Note `input_schema`, not the OpenAI dialect's nested
// `function.parameters` — the schema itself is the same JSON Schema, only the envelope
// differs.
type wireTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// anthropicImageMediaTypes maps the boundary's bare format onto the media type Anthropic
// expects. An unknown format is DROPPED rather than sent: Anthropic rejects the whole
// request over one bad part, and losing an image is better than losing the answer. Same
// decision, and the same reasoning, as bedrock.bedrockImageFormats.
var anthropicImageMediaTypes = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"jpg":  "image/jpeg", // OpenAI-dialect clients commonly send "jpg"
	"gif":  "image/gif",
	"webp": "image/webp",
}

// toWireTools converts the boundary's tools into Anthropic declarations.
func toWireTools(tools []ports.ToolDef) []wireTool {
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema := t.Parameters
		if schema == nil {
			// A tool with no parameters still needs a schema: Anthropic rejects a missing
			// input_schema, and an empty object is the correct way to say "no arguments".
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// imageBlocks converts the boundary's already-decoded image parts into Anthropic blocks.
func imageBlocks(images []ports.ImagePart) []wireBlock {
	var out []wireBlock
	for _, img := range images {
		mt, ok := anthropicImageMediaTypes[strings.ToLower(img.Format)]
		if !ok || len(img.Bytes) == 0 {
			continue
		}
		out = append(out, wireBlock{Type: "image", Source: &wireImageSource{
			Type: "base64", MediaType: mt, Data: base64.StdEncoding.EncodeToString(img.Bytes),
		}})
	}
	return out
}

// splitSystem separates system messages (concatenated) from the conversation. Same
// logic as the original, now over ports.Message (Text is the textual projection of content).
func splitSystem(msgs []ports.Message) (string, []ports.Message) {
	var sys []string
	var conv []ports.Message
	for _, m := range msgs {
		if m.Role == "system" {
			if m.Text != "" {
				sys = append(sys, m.Text)
			}
			continue
		}
		conv = append(conv, m)
	}
	return strings.Join(sys, "\n"), conv
}

// toWireMessages translates the boundary conversation into Anthropic's Messages format.
//
// The one non-obvious rule, and the same one Bedrock's convertMessages documents: in the
// OpenAI dialect every tool result is its OWN message (`role:"tool"` + tool_call_id), while
// Anthropic requires ALL the tool_result blocks of a turn to arrive GROUPED inside a single
// `user` message, in the order of the assistant's tool_use blocks. Emitting one user message
// per result gets the conversation rejected.
func toWireMessages(msgs []ports.Message) []wireMsg {
	out := make([]wireMsg, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case "tool":
			// Merge this and every following `tool` message into one user turn.
			var results []wireBlock
			for ; i < len(msgs) && msgs[i].Role == "tool"; i++ {
				tm := msgs[i]
				if tm.ToolCallID == "" {
					continue
				}
				results = append(results, wireBlock{
					Type: "tool_result", ToolUseID: tm.ToolCallID, ResultContent: tm.Text,
				})
			}
			i-- // the outer loop increments; step back so the next message is not skipped
			if len(results) > 0 {
				out = append(out, wireMsg{Role: "user", Content: results})
			}

		case "assistant":
			var blocks []wireBlock
			if m.Text != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				args := map[string]interface{}{}
				if tc.Arguments != "" {
					// A malformed argument string becomes an empty object rather than
					// failing the request: the model produced it, and refusing the whole
					// conversation over it would strand a session that could still recover.
					json.Unmarshal([]byte(tc.Arguments), &args)
				}
				blocks = append(blocks, wireBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: args,
				})
			}
			if len(blocks) == 0 {
				continue // an assistant turn with neither text nor tool calls carries nothing
			}
			out = append(out, wireMsg{Role: "assistant", Content: blocks})

		default:
			// user (and anything unrecognised, treated as user). Text first, then images —
			// the order the model reads them in.
			var blocks []wireBlock
			if m.Text != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: m.Text})
			}
			blocks = append(blocks, imageBlocks(m.Images)...)
			if len(blocks) == 0 {
				continue
			}
			out = append(out, wireMsg{Role: "user", Content: blocks})
		}
	}
	return out
}

func (a *Adapter) Invoke(ctx context.Context, in ports.InvokeInput) (ports.Result, error) {
	// Same builder as OpenStream (see stream.go), with stream=false. Sharing it is what
	// keeps the two wire formats from drifting — the prompt-caching cache_control block
	// in particular, where a difference would show up as a silent change in cost rather
	// than as an error.
	req, err := a.buildRequest(ctx, in, false)
	if err != nil {
		return ports.Result{}, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return ports.Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return ports.Result{}, fmt.Errorf("%s %d: %s", a.Transport.label(), resp.StatusCode, string(body))
	}
	var d struct {
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			// Encrypted thinking; the payload is never readable and serves only as a marker.
			Data string `json:"data"`
			// type=tool_use
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &d); err != nil || len(d.Content) == 0 {
		return ports.Result{}, fmt.Errorf("bad %s response", a.Transport.label())
	}
	res := ports.Result{
		InputTokens: d.Usage.InputTokens, OutputTokens: d.Usage.OutputTokens,
		CacheReadInputTokens: d.Usage.CacheReadInputTokens, CacheWriteInputTokens: d.Usage.CacheCreationInputTokens,
		CacheCounters: ports.CacheCountersAbsent,
	}
	// EVERY block, dispatched by type — not Content[0].
	//
	// Reading only the first block is a bug that stayed invisible while thinking was
	// impossible to request: a plain answer has exactly one text block. With thinking
	// enabled the response is `[{type:"thinking"...},{type:"text"...}]`, and a thinking block
	// carries no `text` field — so the old code would have returned an EMPTY answer for every
	// reasoning request. Not "reasoning dropped": no answer at all.
	for _, b := range d.Content {
		switch b.Type {
		case "thinking":
			res.Reasoning += b.Thinking
			// Opaque metadata, never content, and the only thing that makes this reasoning
			// chain continuable on a later turn.
			if b.Signature != "" {
				res.ReasoningSignature = b.Signature
			}
		case "redacted_thinking":
			// The model thought and the provider encrypted it. Recorded so "did not think"
			// stays distinguishable from "not shown to us".
			res.ReasoningRedacted = true
		case "tool_use":
			// Arguments go back as a STRING, because that is what the OpenAI dialect the
			// client speaks expects in `function.arguments`. `{}` when the model sent no
			// input, never an empty string, which some SDKs fail to parse.
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			res.ToolCalls = append(res.ToolCalls, ports.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		default:
			// "text", and anything new that carries prose. Concatenated rather than
			// replaced: a multi-block answer used to lose everything after the first.
			res.Text += b.Text
		}
	}
	res.ReasoningChars = len(res.Reasoning)
	// stop_reason was dropped before. It is what tells the caller the model stopped to CALL a
	// tool rather than because it finished, and the OpenAI-dialect finish_reason is derived
	// from it — without it a tool call looked like a completed answer.
	res.StopReason = d.StopReason
	// In Anthropic's native API, input_tokens EXCLUDES the cached ones: they come in
	// dedicated fields. Hence this provider's convention is exclusive.
	if res.CacheReadInputTokens > 0 || res.CacheWriteInputTokens > 0 {
		res.CacheCounters = ports.CacheCountersReported
	}
	return res, nil
}
