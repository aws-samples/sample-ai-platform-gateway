// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Regression for the semantic cache false positive.
//
// A real bug observed in an app with a long system prompt: the question "qual o maior
// clube de futebol de São Paulo?" received the answer to "qual o nome do maior banco
// do Brasil?". Cause: the vectorized text included ALL messages, so the system prompt
// (~930 chars) dominated the question (~37 chars) and the embedding ended up
// representing the prompt, not the question.
//
// Measured with Titan v2 (threshold 0.92):
//
//	                        question only   system+question
//	bank × football               0.29            0.96  ← false positive
//	bank × bank (typo)            0.95            0.99
//
// These tests pin both halves of the fix: what gets vectorized (SemQueryText) and the
// partitioning by context (SemContextKey).
package routing

import (
	"strings"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

const sysPromptLongo = "Você é o assistente do BD Customer Intelligence, uma plataforma interna " +
	"que centraliza inteligência de clientes para times de Business Development. Seu papel é " +
	"ajudar o usuário a entender a carteira, cruzar engajamentos, tecnologias, setores e regiões. " +
	"Sempre conecte a pergunta a um cliente da carteira ou a uma oportunidade concreta. Ofereça " +
	"próximos passos acionáveis. Responda em português do Brasil, de forma concisa e profissional."

func msgs(pairs ...string) []ports.Message {
	out := make([]ports.Message, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, ports.Message{Role: pairs[i], Text: pairs[i+1]})
	}
	return out
}

// The system prompt must NOT enter the vectorized text — that is what caused the bug.
func TestSemQueryText_ExcluiSystemPrompt(t *testing.T) {
	got := SemQueryText(msgs(
		"system", sysPromptLongo,
		"user", "qual o maior clube de futebol de São Paulo?",
	))
	if strings.Contains(got, "assistente") || strings.Contains(got, "carteira") {
		t.Fatalf("the system prompt leaked into the vectorized text: %q", got)
	}
	if got != "qual o maior clube de futebol de sao paulo" {
		t.Errorf("text = %q", got)
	}
}

// An assistant turn is an ANSWER, not a question: including it would make a long
// conversation converge onto itself and inflate the similarity between any two turns.
func TestSemQueryText_ExcluiTurnoDoAssistente(t *testing.T) {
	got := SemQueryText(msgs(
		"system", "seja breve",
		"user", "quanto é 2+2?",
		"assistant", "quatro",
		"user", "e 3+3?",
	))
	if strings.Contains(got, "quatro") {
		t.Fatalf("the assistant's answer leaked: %q", got)
	}
	if got != "quanto e 2+2\ne 3+3" {
		t.Errorf("text = %q", got)
	}
}

// The proof of the bug as a test: two UNRELATED questions, same system prompt. The
// vectorized text must be DIFFERENT (it used to be practically identical).
func TestSemQueryText_PerguntasDistintasNaoColapsam(t *testing.T) {
	banco := SemQueryText(msgs("system", sysPromptLongo, "user", "qual o nome do maior banco do brasil?"))
	futebol := SemQueryText(msgs("system", sysPromptLongo, "user", "qual o maior clube de futebol de Sao Paulo?"))

	if banco == futebol {
		t.Fatal("unrelated questions produced the SAME vectorized text")
	}
	// The constant part (the prompt) must not make up most of the text — that is where
	// the signal dilution came from.
	if len(banco) > 120 {
		t.Errorf("vectorized text too long (%d chars): the prompt probably got in", len(banco))
	}
}

func TestSemQueryText_SemTurnoDeUsuarioDaVazio(t *testing.T) {
	if got := SemQueryText(msgs("system", sysPromptLongo)); got != "" {
		t.Errorf("with no user question it should be empty, got %q", got)
	}
}

// --- Partitioning by context ---

// Same system prompt and same model → same partition.
func TestSemContextKey_Estavel(t *testing.T) {
	a := SemContextKey(msgs("system", sysPromptLongo, "user", "x"), "claude-sonnet")
	b := SemContextKey(msgs("system", sysPromptLongo, "user", "outra pergunta"), "claude-sonnet")
	if a != b {
		t.Error("the user's question should not affect the context partition")
	}
}

// A different system prompt → a different partition. Two distinct assistants must not
// share a response, even for an identical question.
func TestSemContextKey_SystemPromptDiferenteSeparaParticao(t *testing.T) {
	a := SemContextKey(msgs("system", "você é um assistente de vendas", "user", "qual o preço?"), "m1")
	b := SemContextKey(msgs("system", "você é um assistente de suporte técnico", "user", "qual o preço?"), "m1")
	if a == b {
		t.Error("different system prompts should produce different partitions")
	}
}

// A different model → a different partition: it changes both the content and the
// attributed cost.
func TestSemContextKey_ModeloDiferenteSeparaParticao(t *testing.T) {
	if SemContextKey(msgs("system", "s"), "m1") == SemContextKey(msgs("system", "s"), "m2") {
		t.Error("different models should produce different partitions")
	}
}

// --- Context filter on the match ---

func vecOf(vals ...float32) []float32 { return vals }

func TestBestSemanticMatch_IgnoraContextoDiferente(t *testing.T) {
	q := vecOf(1, 0, 0)
	cands := []SemCandidate{
		{CacheKey: "outro-assistente", Vec: vecOf(1, 0, 0), Ctx: "ctx-B", Num: "-"}, // identical, but another persona
	}
	if _, ok := BestSemanticMatch(q, cands, 0.9, "ctx-A", "-"); ok {
		t.Error("a candidate from another context must not match, not even with an identical vector")
	}
	cands[0].Ctx = "ctx-A"
	if _, ok := BestSemanticMatch(q, cands, 0.9, "ctx-A", "-"); !ok {
		t.Error("same context and an identical vector should match")
	}
}

// Entries written BEFORE the fix have an empty Ctx and a vector computed over the whole
// system prompt — unusable. They must be ignored, otherwise the bug survives the deploy
// until the index TTL expires.
func TestBestSemanticMatch_IgnoraEntradaAntigaSemContexto(t *testing.T) {
	q := vecOf(1, 0, 0)
	cands := []SemCandidate{{CacheKey: "envenenada", Vec: vecOf(1, 0, 0), Ctx: ""}}
	if _, ok := BestSemanticMatch(q, cands, 0.9, "ctx-A", "-"); ok {
		t.Error("an entry with no context (pre-fix) must not match")
	}
}

// The floor still applies: a weak match is never served.
func TestBestSemanticMatch_RespeitaThreshold(t *testing.T) {
	q := vecOf(1, 0)
	cands := []SemCandidate{{CacheKey: "fraco", Vec: vecOf(0.3, 0.95), Ctx: "c", Num: "-"}}
	if _, ok := BestSemanticMatch(q, cands, 0.92, "c", "-"); ok {
		t.Error("a similarity below the threshold must not match")
	}
}

// --- Numeric guard -------------------------------------------------------------
//
// Titan v2 measurements that motivated the guard. Pairs that MUST be treated as
// different questions, and the similarity the embedding gives:
//
//	60 rpm      × 600 rpm       0.930  ← CROSSES the 0.92 threshold
//	1 hour      × 24 hours      0.908
//	100 dollars × 1000 dollars  0.893
//
// Whereas a LEGITIMATE paraphrase measures 0.40–0.90. The classes overlap: similarity
// drops MORE with a legitimate rewording than with a number swap. Therefore no global
// threshold separates them — the guard has to be deterministic.

func TestNumFingerprint_NumerosDiferentesDaoImpressaoDiferente(t *testing.T) {
	casos := [][2]string{
		{"o limite e de 60 requisicoes por minuto", "o limite e de 600 requisicoes por minuto"},
		{"o cache expira em 1 hora", "o cache expira em 24 horas"},
		{"meu budget e de 100 dolares", "meu budget e de 1000 dolares"},
	}
	for _, c := range casos {
		if NumFingerprint(c[0]) == NumFingerprint(c[1]) {
			t.Errorf("different numbers should produce different fingerprints:\n  %q\n  %q", c[0], c[1])
		}
	}
}

// The order of the numbers in the text must not matter — the SET is what identifies.
func TestNumFingerprint_OrdemNaoImporta(t *testing.T) {
	if NumFingerprint("de 10 a 20 requisicoes") != NumFingerprint("de 20 a 10 requisicoes") {
		t.Error("the set of numbers is the same; the fingerprint should match")
	}
}

// A decimal comma and a decimal point are the same quantity (PT-BR × EN).
func TestNumFingerprint_DecimalPtBrEqualeAoEn(t *testing.T) {
	if NumFingerprint("custa 1,50 por mil tokens") != NumFingerprint("custa 1.50 por mil tokens") {
		t.Error("1,50 and 1.50 are the same quantity")
	}
}

// Text with no number has a stable fingerprint (most questions land here, and they must
// keep being able to match).
func TestNumFingerprint_SemNumero(t *testing.T) {
	a := NumFingerprint("qual o maior banco do brasil")
	b := NumFingerprint("que banco e o maior do brasil")
	if a != "-" || b != "-" {
		t.Errorf("text with no number should give '-': %q / %q", a, b)
	}
	if a != b {
		t.Error("two questions with no number must be able to match")
	}
}

// The test that closes the bug: practically identical vectors (which is what the
// embedding returns for 60 × 600) must NOT match when the numbers differ.
func TestBestSemanticMatch_GuardaNumericaBloqueiaTrocaDeNumero(t *testing.T) {
	q := vecOf(1, 0, 0)
	const ctx = "ctx-A"
	num60 := NumFingerprint("o limite e de 60 requisicoes por minuto")
	num600 := NumFingerprint("o limite e de 600 requisicoes por minuto")

	// Similarity 1.0 (the worst case) but different numbers → refuse.
	cands := []SemCandidate{{CacheKey: "resposta-de-600", Vec: vecOf(1, 0, 0), Ctx: ctx, Num: num600}}
	if m, ok := BestSemanticMatch(q, cands, 0.92, ctx, num60); ok {
		t.Fatalf("a number swap must not match (score %.4f)", m.Score)
	}
	// Same number → matching is possible again.
	cands[0].Num = num60
	if _, ok := BestSemanticMatch(q, cands, 0.92, ctx, num60); !ok {
		t.Error("the same set of numbers should allow a match")
	}
}

// Questions with no number at all keep working (we must not have broken the common case
// while closing the numeric one).
func TestBestSemanticMatch_SemNumeroContinuaCasando(t *testing.T) {
	semNum := NumFingerprint("qual o maior banco do brasil")
	cands := []SemCandidate{{CacheKey: "k", Vec: vecOf(1, 0), Ctx: "c", Num: semNum}}
	if _, ok := BestSemanticMatch(vecOf(1, 0), cands, 0.9, "c", semNum); !ok {
		t.Error("a question with no number should match normally")
	}
}
