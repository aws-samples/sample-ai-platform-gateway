// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import "testing"

func TestDeriveModelActions_Adicionar(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"m1":{"provider":"bedrock"}}}`),
		obj(`{"routing":{"m1":{"provider":"bedrock"},"novo":{"provider":"openai_compatible","base_url":"https://x"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 1 {
		t.Fatalf("actions = %d, expected 1: %+v", len(acts), acts)
	}
	if acts[0].Action != ActionModelAdd || acts[0].Model != "novo" {
		t.Errorf("action = %q model = %q, expected model_add/novo", acts[0].Action, acts[0].Model)
	}
}

func TestDeriveModelActions_Remover(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"m1":{"provider":"bedrock"},"velho":{"provider":"anthropic"}}}`),
		obj(`{"routing":{"m1":{"provider":"bedrock"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 1 {
		t.Fatalf("actions = %d, expected 1: %+v", len(acts), acts)
	}
	if acts[0].Action != ActionModelRemove || acts[0].Model != "velho" {
		t.Errorf("action = %q model = %q, expected model_remove/velho", acts[0].Action, acts[0].Model)
	}
}

func TestDeriveModelActions_Alterar(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"m1":{"provider":"bedrock","base_url":"https://a"}}}`),
		obj(`{"routing":{"m1":{"provider":"bedrock","base_url":"https://b"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 1 || acts[0].Action != ActionModelUpdate || acts[0].Model != "m1" {
		t.Fatalf("expected model_update/m1: %+v", acts)
	}
}

// A change outside "routing" generates no model event — otherwise any budget PUT would
// produce noise in the Models sub-tab.
func TestDeriveModelActions_MudancaIrrelevante(t *testing.T) {
	chs := Diff(obj(`{"budget":{"limit_usd":100}}`), obj(`{"budget":{"limit_usd":50}}`))
	if acts := DeriveModelActions(chs); len(acts) != 0 {
		t.Errorf("a change outside routing should not generate an action: %+v", acts)
	}
}

// Several models in one request generate several events, in deterministic order.
func TestDeriveModelActions_VariosModelosOrdemDeterministica(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"zeta":{"provider":"a"},"alfa":{"provider":"a"}}}`),
		obj(`{"routing":{"zeta":{"provider":"b"},"alfa":{"provider":"b"},"meio":{"provider":"c"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 3 {
		t.Fatalf("actions = %d, expected 3: %+v", len(acts), acts)
	}
	if acts[0].Model != "alfa" || acts[1].Model != "meio" || acts[2].Model != "zeta" {
		t.Errorf("order = %q,%q,%q — expected alphabetical", acts[0].Model, acts[1].Model, acts[2].Model)
	}
	if acts[1].Action != ActionModelAdd {
		t.Errorf("a new model should be model_add, got %q", acts[1].Action)
	}
}

// A model name with a dot (gpt-4.1) is a real case and would break a naive split on
// ".", attributing the change to a model "gpt-4" that does not exist.
func TestDeriveModelActions_NomeDeModeloComPonto(t *testing.T) {
	chs := Diff(
		obj(`{"routing":{"gpt-4.1":{"provider":"openai_compatible"}}}`),
		obj(`{"routing":{"gpt-4.1":{"provider":"azure"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 1 {
		t.Fatalf("actions = %d, expected 1: %+v", len(acts), acts)
	}
	if acts[0].Model != "gpt-4.1" {
		t.Errorf("model = %q, expected gpt-4.1", acts[0].Model)
	}
}

// Regression for a real bug found during the first end-to-end check: the api_key field
// was not in the anchor list, so "routing.m.api_key" became a "model" named "m.api_key"
// — the model was split into TWO events, one of them with a made-up name. A route field
// missing from the list causes exactly that.
func TestDeriveModelActions_TodosOsCamposDeRotaResolvemOMesmoModelo(t *testing.T) {
	chs := Diff(obj(`{}`), obj(`{"routing":{"m1":{
	   "provider":"openai_compatible","provider_model_id":"x","base_url":"https://a",
	   "api_key":"sk-1","api_key_secret":"nome","region":"us-east-1","kind":"external",
	   "role_arn":"arn:x","external_id":"e","enabled":true}}}`))
	acts := DeriveModelActions(chs)
	if len(acts) != 1 {
		names := make([]string, len(acts))
		for i, a := range acts {
			names[i] = a.Model
		}
		t.Fatalf("all fields should land on the same model, got %d: %v", len(acts), names)
	}
	if acts[0].Model != "m1" {
		t.Errorf("model = %q, expected m1", acts[0].Model)
	}
}

// api_key_secret must be tested before api_key in the anchor list, otherwise the shorter
// prefix matches first and the model name comes out truncated.
func TestDeriveModelActions_ApiKeySecretNaoTruncaONome(t *testing.T) {
	chs := Diff(obj(`{}`), obj(`{"routing":{"meu-modelo":{"api_key_secret":"s"}}}`))
	acts := DeriveModelActions(chs)
	if len(acts) != 1 || acts[0].Model != "meu-modelo" {
		t.Fatalf("expected model 'meu-modelo': %+v", acts)
	}
}

// Each action's changes carry only what belongs to that model — the record must not drag
// along the whole config diff.
func TestDeriveModelActions_MudancasEscopadasAoModelo(t *testing.T) {
	chs := Diff(
		obj(`{"budget":{"limit_usd":1},"routing":{"a":{"provider":"x"},"b":{"provider":"x"}}}`),
		obj(`{"budget":{"limit_usd":2},"routing":{"a":{"provider":"y"},"b":{"provider":"z"}}}`),
	)
	acts := DeriveModelActions(chs)
	if len(acts) != 2 {
		t.Fatalf("actions = %d: %+v", len(acts), acts)
	}
	for _, a := range acts {
		if len(a.Changes) != 1 {
			t.Errorf("model %q carried %d changes, expected 1", a.Model, len(a.Changes))
		}
	}
}
