// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package smsecrets implements ports.SecretStore against AWS Secrets Manager,
// preserving config-api's current mechanism: CreateSecret and, when it already
// exists, PutSecretValue. The value is stored as {"api_key": <apiKey>}.
package smsecrets

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aiplat/governance/internal/ports"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// SM is the subset of the Secrets Manager client the adapter uses (it makes a
// test double easy, although the port's primary double lives in
// internal/adapters/inmem).
type SM interface {
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type Adapter struct{ sm SM }

func New(sm SM) *Adapter { return &Adapter{sm: sm} }

var _ ports.SecretStore = (*Adapter)(nil)

func (a *Adapter) Put(ctx context.Context, name, apiKey string) (string, error) {
	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	s := string(body)
	_, err := a.sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: &name, SecretString: &s})
	if err != nil {
		var exists *smtypes.ResourceExistsException
		if errors.As(err, &exists) {
			_, err = a.sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: &name, SecretString: &s})
		}
	}
	return name, err
}

// Get reads the secret's api_key (value stored as {"api_key": ...}). It also
// accepts a plain-text secret (fallback) for compatibility with credentials
// stored outside the convention. Errors when absent/empty.
func (a *Adapter) Get(ctx context.Context, name string) (string, error) {
	out, err := a.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &name})
	if err != nil {
		return "", err
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", errors.New("secret is empty")
	}
	var m map[string]string
	if json.Unmarshal([]byte(*out.SecretString), &m) == nil {
		if k := m["api_key"]; k != "" {
			return k, nil
		}
	}
	return *out.SecretString, nil // plain text
}
