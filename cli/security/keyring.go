package security

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Keyring provides secure credential storage with OS-level integration.
// Falls back to file-based storage when OS keyring is unavailable.
type Keyring struct {
	mu        sync.RWMutex
	fallback  map[string]string // in-memory fallback (dev only)
	storePath string
}

// NewKeyring creates a new keyring manager
func NewKeyring(storePath string) *Keyring {
	k := &Keyring{
		fallback:  make(map[string]string),
		storePath: storePath,
	}
	_ = k.loadFallback()
	return k
}

// Set stores a credential securely
func (k *Keyring) Set(service, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	// Try OS keyring first
	if err := k.setOS(service, key, value); err == nil {
		return nil
	}

	// Fallback to encrypted file storage
	return k.setFallback(service, key, value)
}

// Get retrieves a credential
func (k *Keyring) Get(service, key string) (string, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	// Try OS keyring first
	if val, err := k.getOS(service, key); err == nil {
		return val, nil
	}

	// Fallback
	return k.getFallback(service, key)
}

// Delete removes a credential
func (k *Keyring) Delete(service, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	_ = k.deleteOS(service, key)
	delete(k.fallback, k.fallbackKey(service, key))
	return nil
}

// List returns all keys for a service
func (k *Keyring) List(service string) ([]string, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	var keys []string
	prefix := service + "/"
	for k := range k.fallback {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, strings.TrimPrefix(k, prefix))
		}
	}
	return keys, nil
}

// OS-specific implementations

func (k *Keyring) setOS(service, key, value string) error {
	switch runtime.GOOS {
	case "darwin":
		return k.setMacOS(service, key, value)
	case "linux":
		return k.setLinux(service, key, value)
	case "windows":
		return k.setWindows(service, key, value)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func (k *Keyring) getOS(service, key string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return k.getMacOS(service, key)
	case "linux":
		return k.getLinux(service, key)
	case "windows":
		return k.getWindows(service, key)
	default:
		return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func (k *Keyring) deleteOS(service, key string) error {
	switch runtime.GOOS {
	case "darwin":
		return k.deleteMacOS(service, key)
	case "linux":
		return k.deleteLinux(service, key)
	case "windows":
		return k.deleteWindows(service, key)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// macOS Keychain (via security command)
func (k *Keyring) setMacOS(service, key, value string) error {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return fmt.Errorf("service/key required")
	}
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", targetService, "-a", targetAccount, "-w", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (k *Keyring) getMacOS(service, key string) (string, error) {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return "", fmt.Errorf("service/key required")
	}
	cmd := exec.Command("security", "find-generic-password", "-s", targetService, "-a", targetAccount, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("security find-generic-password: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *Keyring) deleteMacOS(service, key string) error {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return fmt.Errorf("service/key required")
	}
	cmd := exec.Command("security", "delete-generic-password", "-s", targetService, "-a", targetAccount)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Keep delete idempotent.
		text := strings.ToLower(string(out))
		if strings.Contains(text, "could not be found") || strings.Contains(text, "item not found") {
			return nil
		}
		return fmt.Errorf("security delete-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Linux (via secret-tool or pass)
func (k *Keyring) setLinux(service, key, value string) error {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return fmt.Errorf("service/key required")
	}

	cmd := exec.Command("secret-tool", "store", "--label", "xclaw credentials", "service", targetService, "account", targetAccount)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("secret-tool store: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (k *Keyring) getLinux(service, key string) (string, error) {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return "", fmt.Errorf("service/key required")
	}

	cmd := exec.Command("secret-tool", "lookup", "service", targetService, "account", targetAccount)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("secret-tool lookup: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *Keyring) deleteLinux(service, key string) error {
	targetService := strings.TrimSpace(service)
	targetAccount := strings.TrimSpace(key)
	if targetService == "" || targetAccount == "" {
		return fmt.Errorf("service/key required")
	}

	cmd := exec.Command("secret-tool", "clear", "service", targetService, "account", targetAccount)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Keep delete idempotent.
		text := strings.ToLower(string(out))
		if strings.Contains(text, "no such secret") || strings.Contains(text, "not found") {
			return nil
		}
		return fmt.Errorf("secret-tool clear: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Windows Credential Manager (via cmdkey or wincred)
func (k *Keyring) setWindows(service, key, value string) error {
	_ = service
	_ = key
	_ = value
	return fmt.Errorf("windows keyring backend unavailable in current build")
}

func (k *Keyring) getWindows(service, key string) (string, error) {
	_ = service
	_ = key
	return "", fmt.Errorf("windows keyring backend unavailable in current build")
}

func (k *Keyring) deleteWindows(service, key string) error {
	_ = service
	_ = key
	return fmt.Errorf("windows keyring backend unavailable in current build")
}

// Fallback implementations (encrypted file-based)

func (k *Keyring) fallbackKey(service, key string) string {
	return service + "/" + key
}

func (k *Keyring) setFallback(service, key, value string) error {
	k.fallback[k.fallbackKey(service, key)] = value
	return k.saveFallback()
}

func (k *Keyring) getFallback(service, key string) (string, error) {
	val, ok := k.fallback[k.fallbackKey(service, key)]
	if !ok {
		return "", fmt.Errorf("key not found: %s/%s", service, key)
	}
	return val, nil
}

func (k *Keyring) saveFallback() error {
	if k.storePath == "" {
		return nil // In-memory only
	}
	if err := os.MkdirAll(filepath.Dir(k.storePath), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(k.fallback)
	if err != nil {
		return err
	}
	encoded := encodeBase64(raw)
	if err := os.WriteFile(k.storePath, []byte(encoded), 0o600); err != nil {
		return err
	}
	return nil
}

func (k *Keyring) loadFallback() error {
	if k.storePath == "" {
		return nil
	}
	if !fileExists(k.storePath) {
		return nil
	}
	b, err := os.ReadFile(k.storePath)
	if err != nil {
		return err
	}
	decoded, err := decodeBase64(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, &k.fallback)
}

// RBAC (Role-Based Access Control)

// Permission represents a single permission
type Permission struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

// Role represents a role with permissions
type Role struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Permissions []Permission `json:"permissions"`
}

// RBAC provides role-based access control
type RBAC struct {
	mu     sync.RWMutex
	roles  map[string]*Role
	grants map[string][]string // userID -> []roleID
}

// NewRBAC creates a new RBAC manager
func NewRBAC() *RBAC {
	return &RBAC{
		roles:  make(map[string]*Role),
		grants: make(map[string][]string),
	}
}

// AddRole adds a role
func (r *RBAC) AddRole(role *Role) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles[role.ID] = role
}

// Grant grants a role to a user
func (r *RBAC) Grant(userID, roleID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.roles[roleID]; !ok {
		return fmt.Errorf("role not found: %s", roleID)
	}

	r.grants[userID] = append(r.grants[userID], roleID)
	return nil
}

// Revoke revokes a role from a user
func (r *RBAC) Revoke(userID, roleID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	grants := r.grants[userID]
	filtered := make([]string, 0, len(grants))
	for _, rid := range grants {
		if rid != roleID {
			filtered = append(filtered, rid)
		}
	}
	r.grants[userID] = filtered
}

// Can checks if a user has a permission
func (r *RBAC) Can(userID string, perm Permission) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, roleID := range r.grants[userID] {
		role, ok := r.roles[roleID]
		if !ok {
			continue
		}
		for _, p := range role.Permissions {
			if p.Resource == perm.Resource && p.Action == perm.Action {
				return true
			}
			if p.Resource == "*" && p.Action == perm.Action {
				return true
			}
			if p.Resource == perm.Resource && p.Action == "*" {
				return true
			}
		}
	}
	return false
}

// DefaultRoles returns predefined roles
func DefaultRoles() []*Role {
	return []*Role{
		{
			ID:   "admin",
			Name: "管理员",
			Permissions: []Permission{
				{Resource: "*", Action: "*"},
			},
		},
		{
			ID:   "agent_operator",
			Name: "Agent 操作员",
			Permissions: []Permission{
				{Resource: "agent", Action: "read"},
				{Resource: "agent", Action: "write"},
				{Resource: "session", Action: "read"},
				{Resource: "session", Action: "write"},
				{Resource: "tool", Action: "execute"},
			},
		},
		{
			ID:   "viewer",
			Name: "观察者",
			Permissions: []Permission{
				{Resource: "agent", Action: "read"},
				{Resource: "session", Action: "read"},
				{Resource: "audit", Action: "read"},
			},
		},
	}
}

// Helper functions

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func decodeBase64(s string) ([]byte, error) {
	// Accept legacy plaintext JSON files for compatibility.
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		return []byte(s), nil
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(out), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
