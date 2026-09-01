// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package repository provides a factory for instantiating the correct storage backend.
package repository

import (
	"context"
	"fmt"
)

// RepositoryFactory creates repository instances based on configuration.
type RepositoryFactory struct {
	Usage UsageRepository
}

// Config holds repository configuration.
type Config struct {
	Backend string // "dynamodb", "postgres", "mongodb"

	// DynamoDB-specific
	DynamoDBTable  string
	DynamoDBRegion string

	// Postgres-specific
	PostgresHost     string
	PostgresPort     int
	PostgresDB       string
	PostgresUser     string
	PostgresPassword string

	// MongoDB-specific
	MongoDBURI string
	MongoDBDB  string
}

// NewRepositoryFactory creates a new repository factory with the specified backend.
func NewRepositoryFactory(ctx context.Context, cfg Config) (*RepositoryFactory, error) {
	var usage UsageRepository
	var err error

	switch cfg.Backend {
	case "dynamodb":
		usage, err = NewDynamoDBUsageRepository(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create DynamoDB repository: %w", err)
		}
	case "postgres":
		usage, err = NewPostgresUsageRepository(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create Postgres repository: %w", err)
		}
	case "mongodb":
		usage, err = NewMongoDBUsageRepository(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create MongoDB repository: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", cfg.Backend)
	}

	return &RepositoryFactory{
		Usage: usage,
	}, nil
}
