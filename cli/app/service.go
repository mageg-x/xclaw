package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	sec "xclaw/cli/crypto"
	"xclaw/cli/db"
	"xclaw/cli/models"
	"xclaw/cli/security"
)

type Service struct {
	store      *db.Store
	iterations int
	keyBytes   int
	keyring    *security.Keyring
	tokenMu    sync.RWMutex
	tokens     map[string]time.Time
}

type BootstrapInput struct {
	MasterPassword string `json:"master_password"`
	Provider       string `json:"provider"`
	DefaultModel   string `json:"default_model"`
	APIKey         string `json:"api_key"`
}

type SetCredentialInput struct {
	Provider       string `json:"provider"`
	Secret         string `json:"secret"`
	MasterPassword string `json:"master_password"`
}

func NewService(store *db.Store, iterations, keyBytes int, keyring *security.Keyring) *Service {
	return &Service{
		store:      store,
		iterations: iterations,
		keyBytes:   keyBytes,
		keyring:    keyring,
		tokens:     make(map[string]time.Time),
	}
}

func (s *Service) IsBootstrapped(ctx context.Context) (bool, error) {
	raw, ok, err := s.store.GetSetting(ctx, "onboarding_done")
	if err != nil || !ok {
		return false, err
	}
	return raw == "true", nil
}

func (s *Service) Bootstrap(ctx context.Context, in BootstrapInput) error {
	if len(in.MasterPassword) < 8 {
		return fmt.Errorf("master password must be at least 8 chars")
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate bootstrap salt: %w", err)
	}

	hash := sec.PBKDF2SHA256([]byte(in.MasterPassword), salt, s.iterations, s.keyBytes)

	if err := s.store.SetSetting(ctx, "master_salt_b64", base64.StdEncoding.EncodeToString(salt)); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, "master_hash_b64", base64.StdEncoding.EncodeToString(hash)); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, "onboarding_done", "true"); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, "bootstrapped_at", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if strings.TrimSpace(in.Provider) != "" {
		if err := s.store.SetSetting(ctx, "default_provider", strings.TrimSpace(in.Provider)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.DefaultModel) != "" {
		if err := s.store.SetSetting(ctx, "default_model", strings.TrimSpace(in.DefaultModel)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.Provider) != "" && strings.TrimSpace(in.APIKey) != "" {
		if err := s.SetCredential(ctx, SetCredentialInput{
			Provider:       strings.TrimSpace(in.Provider),
			Secret:         strings.TrimSpace(in.APIKey),
			MasterPassword: in.MasterPassword,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Login(ctx context.Context, password string) (string, error) {
	ok, err := s.VerifyMasterPassword(ctx, password)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("invalid password")
	}

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(raw)

	s.tokenMu.Lock()
	s.tokens[token] = time.Now().Add(24 * time.Hour)
	s.tokenMu.Unlock()
	return token, nil
}

func (s *Service) ValidateToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	now := time.Now()

	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	exp, ok := s.tokens[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *Service) Logout(token string) {
	s.tokenMu.Lock()
	delete(s.tokens, strings.TrimSpace(token))
	s.tokenMu.Unlock()
}

func (s *Service) VerifyMasterPassword(ctx context.Context, password string) (bool, error) {
	saltB64, ok, err := s.store.GetSetting(ctx, "master_salt_b64")
	if err != nil || !ok {
		return false, err
	}
	hashB64, ok, err := s.store.GetSetting(ctx, "master_hash_b64")
	if err != nil || !ok {
		return false, err
	}

	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	hash, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}

	computed := sec.PBKDF2SHA256([]byte(password), salt, s.iterations, len(hash))
	return subtle.ConstantTimeCompare(hash, computed) == 1, nil
}

func (s *Service) SetCredential(ctx context.Context, in SetCredentialInput) error {
	if in.Provider == "" || in.Secret == "" {
		return fmt.Errorf("provider and secret are required")
	}
	ok, err := s.VerifyMasterPassword(ctx, in.MasterPassword)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invalid master password")
	}

	// Try keyring first, fallback to SQLite
	if s.keyring != nil {
		if err := s.keyring.Set("credentials", in.Provider, in.Secret); err == nil {
			return nil
		}
	}

	payload, err := sec.EncryptSecret(in.MasterPassword, in.Secret, s.iterations, s.keyBytes)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	cred := models.Credential{
		Provider:      in.Provider,
		CiphertextB64: payload.CiphertextB64,
		NonceB64:      payload.NonceB64,
		SaltB64:       payload.SaltB64,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	return s.store.UpsertCredential(ctx, cred)
}

func (s *Service) GetCredential(ctx context.Context, provider, masterPassword string) (string, error) {
	ok, err := s.VerifyMasterPassword(ctx, masterPassword)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("invalid master password")
	}

	// Try keyring first, fallback to SQLite
	if s.keyring != nil {
		if val, err := s.keyring.Get("credentials", provider); err == nil {
			return val, nil
		}
	}

	cred, err := s.store.GetCredential(ctx, provider)
	if err != nil {
		return "", err
	}

	return sec.DecryptSecret(masterPassword, sec.EncryptedPayload{
		CiphertextB64: cred.CiphertextB64,
		NonceB64:      cred.NonceB64,
		SaltB64:       cred.SaltB64,
	}, s.iterations, s.keyBytes)
}
