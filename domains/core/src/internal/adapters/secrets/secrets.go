// Package secrets is the outbound adapter for AWS Secrets Manager.
//
// Feature: hexagonal-refactor, task 4.6. Code MOVED from cmd/router/main.go
// (getSecret, resolveSecretName, fetchSecret) with security enhancements.
// Single-org model: BYO per-org credential support removed. Secrets are now
// deployment-level. Cache keyed by secret name with encrypted storage and 1 min TTL.
// Security: Secrets encrypted in memory (AES-256-GCM), secure zeroing on eviction.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/aiplat/core/internal/ports"
)

// SecretEntry holds an encrypted secret with metadata
type SecretEntry struct {
	encrypted []byte // Encrypted secret
	nonce     []byte // Nonce for decryption
	timestamp time.Time
}

// Store is the production adapter over Secrets Manager with secure caching.
type Store struct {
	sm        *secretsmanager.Client
	mu        sync.RWMutex
	cache     map[string]SecretEntry
	masterKey []byte // Derived from environment or KMS
	gcm       cipher.AEAD
	maxAge    time.Duration
}

var _ ports.SecretStore = (*Store)(nil) // compile-time assertion

// New builds the adapter with the Secrets Manager client and a master encryption key.
// The master key must be 32 bytes (256 bits) for AES-256.
func New(sm *secretsmanager.Client) *Store {
	// Generate a random master key for this session
	// In production, this should come from KMS or environment variable
	masterKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, masterKey); err != nil {
		panic(fmt.Sprintf("failed to generate master key: %v", err))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(fmt.Sprintf("failed to create cipher: %v", err))
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("failed to create GCM: %v", err))
	}

	return &Store{
		sm:        sm,
		cache:     make(map[string]SecretEntry),
		masterKey: masterKey,
		gcm:       gcm,
		maxAge:    1 * time.Minute, // Reduced from 5 minutes
	}
}

// encrypt encrypts a secret using AES-256-GCM
func (s *Store) encrypt(plaintext string) ([]byte, []byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}

	ciphertext := s.gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// decrypt decrypts a secret using AES-256-GCM
func (s *Store) decrypt(ciphertext, nonce []byte) (string, error) {
	plaintext, err := s.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// secureZero overwrites sensitive data in memory
func secureZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Get fetches the credential from the deployment's secret store.
// Returns empty string on error for backward compatibility.
// Single-org model: org parameter removed, secrets are deployment-level.
func (s *Store) Get(ctx context.Context, name string) string {
	secret, err := s.fetch(ctx, name)
	if err != nil {
		// Log detailed error internally
		log.Printf("ERROR: Failed to fetch secret: %v", err)
		// Maintain backward compatibility: silent failure returns ""
		return ""
	}
	return secret
}

func (s *Store) fetch(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("secret name is required")
	}

	// Check cache with read lock
	s.mu.RLock()
	if entry, ok := s.cache[name]; ok {
		if time.Since(entry.timestamp) < s.maxAge {
			s.mu.RUnlock()
			// Decrypt and return
			secret, err := s.decrypt(entry.encrypted, entry.nonce)
			if err != nil {
				return "", fmt.Errorf("failed to decrypt cached secret: %w", err)
			}
			return secret, nil
		}
	}
	s.mu.RUnlock()

	// Fetch from Secrets Manager
	out, err := s.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &name,
	})
	if err != nil {
		// Log detailed error internally (without secret name for security)
		log.Printf("ERROR: Secrets Manager API call failed: %v", err)
		// Return generic error (actual error logged above)
		return "", fmt.Errorf("secret retrieval failed")
	}

	if out.SecretString == nil {
		return "", fmt.Errorf("secret not found")
	}

	// Parse secret
	var secret string
	var m map[string]string
	if json.Unmarshal([]byte(*out.SecretString), &m) == nil && m["api_key"] != "" {
		secret = m["api_key"]
	} else {
		secret = *out.SecretString
	}

	// Encrypt before caching
	encrypted, nonce, err := s.encrypt(secret)
	if err != nil {
		// Still return the secret but don't cache
		return secret, nil
	}

	// Store encrypted in cache with write lock
	s.mu.Lock()
	// Evict old entry if it exists and zero its memory
	if oldEntry, exists := s.cache[name]; exists {
		secureZero(oldEntry.encrypted)
		secureZero(oldEntry.nonce)
	}
	s.cache[name] = SecretEntry{
		encrypted: encrypted,
		nonce:     nonce,
		timestamp: time.Now(),
	}
	s.mu.Unlock()

	return secret, nil
}

// Clear removes a secret from cache and zeroes memory
func (s *Store) Clear(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.cache[name]; ok {
		// Zero out encrypted data
		secureZero(entry.encrypted)
		secureZero(entry.nonce)
		delete(s.cache, name)
	}
}

// ClearAll removes all secrets and zeroes memory
func (s *Store) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for name, entry := range s.cache {
		secureZero(entry.encrypted)
		secureZero(entry.nonce)
		delete(s.cache, name)
	}
}

// StartGarbageCollector removes expired entries periodically
func (s *Store) StartGarbageCollector(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.ClearAll()
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for name, entry := range s.cache {
				if now.Sub(entry.timestamp) > s.maxAge {
					secureZero(entry.encrypted)
					secureZero(entry.nonce)
					delete(s.cache, name)
				}
			}
			s.mu.Unlock()
		}
	}
}
