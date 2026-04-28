package skills

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCatalogURL = "https://raw.githubusercontent.com/xclaw/skills-market/main/catalog.json"
	remoteCacheTTL    = 5 * time.Minute
	installLockName   = ".xclaw-lock.json"
)

type Item struct {
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Description  string    `json:"description"`
	Source       string    `json:"source"`
	SkillURL     string    `json:"skill_url,omitempty"`
	ResolvedFrom string    `json:"resolved_from,omitempty"`
	Integrity    string    `json:"integrity,omitempty"`
	InstalledAt  time.Time `json:"installed_at"`
}

type InstallOptions struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Constraint string `json:"constraint,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	Checksum   string `json:"checksum,omitempty"`
}

type InstallLock struct {
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	Source       string                `json:"source"`
	ResolvedFrom string                `json:"resolved_from,omitempty"`
	Integrity    string                `json:"integrity,omitempty"`
	InstalledAt  time.Time             `json:"installed_at"`
	Files        []string              `json:"files,omitempty"`
	Dependencies []string              `json:"dependencies,omitempty"`
	Manifest     *SkillPackageManifest `json:"manifest,omitempty"`
}

type Market struct {
	mu               sync.Mutex
	skillsDir        string
	storePath        string
	catalog          []Item
	remoteURL        string
	remoteCatalog    []Item
	remoteCatalogAt  time.Time
	remoteCatalogErr string
	httpClient       *http.Client
}

func NewMarket(skillsDir string) *Market {
	catalog := []Item{
		{Name: "code-review", Version: "1.0.0", Description: "规范化代码审查能力", Source: "builtin"},
		{Name: "ops-monitor", Version: "1.0.0", Description: "运维巡检与告警能力", Source: "builtin"},
		{Name: "reporting", Version: "1.0.0", Description: "自动报告生成能力", Source: "builtin"},
		{Name: "a2a-bridge", Version: "1.0.0", Description: "跨实例协作桥接能力", Source: "builtin"},
	}

	remoteURL := strings.TrimSpace(os.Getenv("XCLAW_SKILL_MARKET_URL"))
	if remoteURL == "" {
		remoteURL = DefaultCatalogURL
	}

	return &Market{
		skillsDir: skillsDir,
		storePath: filepath.Join(skillsDir, "installed.json"),
		catalog:   catalog,
		remoteURL: remoteURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (m *Market) MarketURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remoteURL
}

func (m *Market) Catalog(query string) []Item {
	base := m.localCatalog()
	remote := m.remoteCatalogWithCache()

	merged := append(base, remote...)
	merged = dedupeCatalog(merged)
	merged = filterCatalog(merged, query)
	sort.Slice(merged, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(merged[i].Name))
		right := strings.ToLower(strings.TrimSpace(merged[j].Name))
		if left == right {
			return strings.ToLower(strings.TrimSpace(merged[i].Version)) < strings.ToLower(strings.TrimSpace(merged[j].Version))
		}
		return left < right
	})
	return merged
}

func (m *Market) ListInstalled() ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readInstalled()
}

func (m *Market) Install(opts InstallOptions) (Item, error) {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Version = strings.TrimSpace(opts.Version)
	opts.Constraint = strings.TrimSpace(opts.Constraint)
	opts.Source = strings.TrimSpace(opts.Source)
	opts.SourceURL = strings.TrimSpace(opts.SourceURL)
	opts.SourcePath = strings.TrimSpace(opts.SourcePath)
	opts.Checksum = normalizeChecksum(opts.Checksum)

	if opts.Name == "" && opts.SourceURL == "" && opts.SourcePath == "" {
		return Item{}, fmt.Errorf("skill name or explicit source is required")
	}
	if m.isBuiltinSkill(opts.Name) || strings.EqualFold(opts.Source, "builtin") {
		return Item{}, fmt.Errorf("builtin skill is always available and cannot be installed")
	}
	if opts.Source == "" {
		switch {
		case opts.SourcePath != "":
			opts.Source = "local"
		case opts.SourceURL != "":
			opts.Source = "url"
		default:
			opts.Source = "market"
		}
	}

	resolvedURL := opts.SourceURL
	requested := requestedConstraint(opts)
	if resolvedURL == "" {
		switch {
		case looksLikeURL(opts.Source):
			resolvedURL = opts.Source
		case looksLikeURL(opts.Name):
			resolvedURL = opts.Name
			if opts.Name == "" {
				opts.Source = "url"
			}
		default:
			if item, ok := m.findSkillItem(opts.Name, requested); ok {
				resolvedURL = item.SkillURL
				if opts.Version == "" && item.Version != "" {
					opts.Version = item.Version
				}
			}
		}
	}

	targetName := firstNonEmpty(opts.Name, slugifySkillName(baseNameFromSource(firstNonEmpty(opts.SourcePath, resolvedURL))))
	if targetName == "" {
		targetName = fmt.Sprintf("skill-%d", time.Now().UnixNano())
	}
	installDir := filepath.Join(m.skillsDir, "installed", targetName)
	_ = os.RemoveAll(installDir)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return Item{}, fmt.Errorf("create skill dir: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(installDir)
		}
	}()

	manifest, files, resolvedFrom, err := m.installIntoDir(installDir, opts, resolvedURL)
	if err != nil {
		return Item{}, err
	}
	if _, _, err := ParseSkillDir(installDir); err != nil {
		return Item{}, fmt.Errorf("parse installed skill: %w", err)
	}

	doc, _, err := ParseSkillDir(installDir)
	if err != nil {
		return Item{}, fmt.Errorf("parse installed skill: %w", err)
	}
	finalName := strings.TrimSpace(doc.Meta.Name)
	if finalName == "" {
		finalName = targetName
	}
	finalDir := filepath.Join(m.skillsDir, "installed", finalName)
	if finalDir != installDir {
		_ = os.RemoveAll(finalDir)
		if err := os.Rename(installDir, finalDir); err != nil {
			return Item{}, fmt.Errorf("rename installed skill dir: %w", err)
		}
		installDir = finalDir
	}

	if manifest != nil {
		for _, dep := range manifest.Dependencies {
			if err := m.installDependency(dep, resolvedFrom); err != nil {
				return Item{}, fmt.Errorf("install dependency %s: %w", dep, err)
			}
		}
	}

	integrity, err := computeDirIntegrity(installDir)
	if err != nil {
		return Item{}, fmt.Errorf("compute skill integrity: %w", err)
	}
	installedAt := time.Now().UTC()
	if err := m.writeInstallLock(installDir, InstallLock{
		Name:         finalName,
		Version:      firstNonEmpty(doc.Meta.Version, opts.Version, "latest"),
		Source:       opts.Source,
		ResolvedFrom: resolvedFrom,
		Integrity:    integrity,
		InstalledAt:  installedAt,
		Files:        dedupeStrings(files),
		Dependencies: cloneStrings(manifestDependencies(manifest)),
		Manifest:     manifest,
	}); err != nil {
		return Item{}, err
	}

	item := Item{
		Name:         finalName,
		Version:      firstNonEmpty(doc.Meta.Version, opts.Version, "latest"),
		Description:  strings.TrimSpace(doc.Meta.Description),
		Source:       opts.Source,
		SkillURL:     firstNonEmpty(resolvedURL, opts.SourceURL),
		ResolvedFrom: resolvedFrom,
		Integrity:    integrity,
		InstalledAt:  installedAt,
	}
	if err := m.upsertInstalled(item); err != nil {
		return Item{}, err
	}

	cleanup = false
	return item, nil
}

func (m *Market) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if m.isBuiltinSkill(name) {
		return fmt.Errorf("builtin skill is always available and cannot be uninstalled")
	}

	skillDir := filepath.Join(m.skillsDir, "installed", name)
	_ = os.RemoveAll(skillDir)

	items, err := m.readInstalled()
	if err != nil {
		return err
	}
	filtered := make([]Item, 0, len(items))
	for _, item := range items {
		if item.Name != name {
			filtered = append(filtered, item)
		}
	}
	return m.writeInstalled(filtered)
}

func (m *Market) installIntoDir(skillDir string, opts InstallOptions, resolvedURL string) (*SkillPackageManifest, []string, string, error) {
	switch {
	case opts.SourcePath != "":
		return m.installFromLocalSource(skillDir, opts.SourcePath, opts.Checksum)
	case resolvedURL != "":
		return m.installFromRemoteURL(skillDir, resolvedURL, opts.Checksum)
	default:
		return nil, nil, "", fmt.Errorf("skill package source not found for %s", opts.Name)
	}
}

func (m *Market) installFromLocalSource(skillDir, sourcePath, checksum string) (*SkillPackageManifest, []string, string, error) {
	absPath, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve local skill path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("stat local skill path: %w", err)
	}
	if info.IsDir() {
		files, err := copyDirContents(absPath, skillDir)
		if err != nil {
			return nil, nil, "", err
		}
		if checksum != "" {
			integrity, err := computeDirIntegrity(skillDir)
			if err != nil {
				return nil, nil, "", err
			}
			if integrity != checksum {
				return nil, nil, "", fmt.Errorf("checksum mismatch: expected %s got %s", checksum, integrity)
			}
		}
		manifest, _, _ := loadManifestFromDir(skillDir)
		return manifest, files, absPath, nil
	}

	body, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read local skill file: %w", err)
	}
	if err := verifyChecksum(body, checksum); err != nil {
		return nil, nil, "", err
	}
	if strings.EqualFold(filepath.Ext(absPath), ".zip") {
		files, err := unzipBytes(body, skillDir)
		if err != nil {
			return nil, nil, "", err
		}
		manifest, _, _ := loadManifestFromDir(skillDir)
		return manifest, files, absPath, nil
	}
	loader := func(asset string) ([]byte, error) {
		return os.ReadFile(filepath.Join(filepath.Dir(absPath), filepath.Clean(asset)))
	}
	manifest, files, err := m.materializePackage(skillDir, absPath, body, loader)
	if err != nil {
		return nil, nil, "", err
	}
	return manifest, files, absPath, nil
}

func (m *Market) installFromRemoteURL(skillDir, sourceURL, checksum string) (*SkillPackageManifest, []string, string, error) {
	body, err := m.fetchBytes(sourceURL, 2<<20)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetch skill: %w", err)
	}
	if err := verifyChecksum(body, checksum); err != nil {
		return nil, nil, "", err
	}
	if strings.EqualFold(filepath.Ext(path.Base(sourceURL)), ".zip") {
		files, err := unzipBytes(body, skillDir)
		if err != nil {
			return nil, nil, "", err
		}
		manifest, _, _ := loadManifestFromDir(skillDir)
		return manifest, files, sourceURL, nil
	}
	loader := func(asset string) ([]byte, error) {
		assetURL, err := resolveSkillAssetURL(sourceURL, asset)
		if err != nil {
			return nil, err
		}
		return m.fetchBytes(assetURL, 512<<10)
	}
	manifest, files, err := m.materializePackage(skillDir, sourceURL, body, loader)
	if err != nil {
		return nil, nil, "", err
	}
	return manifest, files, sourceURL, nil
}

func (m *Market) materializePackage(skillDir, sourceRef string, body []byte, loadAsset func(string) ([]byte, error)) (*SkillPackageManifest, []string, error) {
	var bundle struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(body, &bundle); err == nil && len(bundle.Files) > 0 {
		files := make([]string, 0, len(bundle.Files))
		for fileName, content := range bundle.Files {
			if err := writeSkillFile(skillDir, fileName, []byte(content)); err != nil {
				return nil, nil, err
			}
			files = append(files, fileName)
		}
		return loadManifestFromDir(skillDir)
	}

	if _, err := ParseSKILLMd(string(body)); err == nil {
		if err := writeSkillFile(skillDir, "SKILL.md", body); err != nil {
			return nil, nil, err
		}
		return nil, []string{"SKILL.md"}, nil
	}

	manifest, fileName, ok := decodeManifestPayload(sourceRef, body)
	if ok {
		files := []string{fileName}
		if err := writeSkillFile(skillDir, fileName, body); err != nil {
			return nil, nil, err
		}
		instructionsFile := strings.TrimSpace(manifest.InstructionsFile)
		if instructionsFile == "" {
			instructionsFile = "instructions.md"
		}
		if content, err := loadAsset(instructionsFile); err == nil && len(strings.TrimSpace(string(content))) > 0 {
			if err := writeSkillFile(skillDir, instructionsFile, content); err != nil {
				return nil, nil, err
			}
			files = append(files, instructionsFile)
		}
		for _, asset := range manifest.Files {
			asset = strings.TrimSpace(asset)
			if asset == "" {
				continue
			}
			content, err := loadAsset(asset)
			if err != nil {
				return nil, nil, fmt.Errorf("fetch skill asset %s: %w", asset, err)
			}
			if err := writeSkillFile(skillDir, asset, content); err != nil {
				return nil, nil, err
			}
			files = append(files, asset)
		}
		return &manifest, dedupeStrings(files), nil
	}

	if strings.Contains(sourceRef, "://") {
		skillMdURL := strings.TrimSuffix(sourceRef, "/") + "/SKILL.md"
		mdBody, err := m.fetchBytes(skillMdURL, 512<<10)
		if err == nil {
			if _, parseErr := ParseSKILLMd(string(mdBody)); parseErr == nil {
				if err := writeSkillFile(skillDir, "SKILL.md", mdBody); err != nil {
					return nil, nil, err
				}
				return nil, []string{"SKILL.md"}, nil
			}
		}
	}

	return nil, nil, fmt.Errorf("unrecognized skill package format")
}

func (m *Market) installDependency(dep, base string) error {
	dep = strings.TrimSpace(dep)
	if dep == "" {
		return nil
	}
	spec := parseDependencySpec(dep)
	opts := InstallOptions{Source: "dependency"}
	switch {
	case looksLikeURL(dep):
		opts.SourceURL = dep
	case looksLikeURL(base):
		resolved, err := resolveSkillAssetURL(base, spec.Raw)
		if err == nil {
			opts.SourceURL = resolved
		} else {
			opts.Name = spec.Name
			opts.Version = spec.Version
			opts.Constraint = spec.Constraint
			opts.Source = "market"
		}
	case base != "":
		root := base
		if info, err := os.Stat(base); err == nil && !info.IsDir() {
			root = filepath.Dir(base)
		}
		candidate := spec.Raw
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(root, spec.Raw)
		}
		if _, err := os.Stat(candidate); err == nil {
			opts.SourcePath = candidate
		} else {
			opts.Name = spec.Name
			opts.Version = spec.Version
			opts.Constraint = spec.Constraint
			opts.Source = "market"
		}
	default:
		opts.Name = spec.Name
		opts.Version = spec.Version
		opts.Constraint = spec.Constraint
		opts.Source = "market"
	}
	_, err := m.Install(opts)
	return err
}

func (m *Market) fetchBytes(sourceURL string, limit int64) ([]byte, error) {
	resp, err := m.httpClient.Get(sourceURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (m *Market) writeInstallLock(skillDir string, lock InstallLock) error {
	body, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install lock: %w", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, installLockName), body, 0o644); err != nil {
		return fmt.Errorf("write install lock: %w", err)
	}
	return nil
}

func (m *Market) upsertInstalled(item Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.readInstalled()
	if err != nil {
		return err
	}
	for i := range items {
		if items[i].Name == item.Name {
			items[i] = item
			return m.writeInstalled(items)
		}
	}
	items = append(items, item)
	return m.writeInstalled(items)
}

func (m *Market) findSkillURL(name string) string {
	item, ok := m.findSkillItem(name, "")
	if !ok {
		return ""
	}
	return item.SkillURL
}

func (m *Market) findSkillItem(name, constraint string) (Item, bool) {
	name = strings.TrimSpace(name)
	constraint = strings.TrimSpace(constraint)
	if name == "" {
		return Item{}, false
	}
	items := m.remoteCatalogWithCache()
	matches := make([]Item, 0, len(items))
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Name), name) {
			continue
		}
		if strings.TrimSpace(item.SkillURL) == "" {
			continue
		}
		if !matchesVersionConstraint(item.Version, constraint) {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) == 0 {
		return Item{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		return compareVersionStrings(matches[i].Version, matches[j].Version) > 0
	})
	return matches[0], true
}

func (m *Market) isBuiltinSkill(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, item := range m.catalog {
		if strings.EqualFold(strings.TrimSpace(item.Name), name) && strings.EqualFold(strings.TrimSpace(item.Source), "builtin") {
			return true
		}
	}
	return false
}

func decodeManifestPayload(sourceURL string, body []byte) (SkillPackageManifest, string, bool) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return SkillPackageManifest{}, "", false
	}
	ext := strings.ToLower(filepath.Ext(sourceURL))
	if strings.HasPrefix(trimmed, "{") {
		var manifest SkillPackageManifest
		if err := json.Unmarshal(body, &manifest); err == nil && strings.TrimSpace(firstNonEmpty(manifest.Name, manifest.ID)) != "" {
			return manifest, "skill.json", true
		}
	}
	manifest := parseYAMLManifest(trimmed)
	if strings.TrimSpace(firstNonEmpty(manifest.Name, manifest.ID)) != "" {
		fileName := "skill.yaml"
		if ext == ".yml" {
			fileName = "skill.yml"
		}
		return manifest, fileName, true
	}
	return SkillPackageManifest{}, "", false
}

func resolveSkillAssetURL(sourceURL, asset string) (string, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(parsed.Path, ".json") || strings.HasSuffix(parsed.Path, ".yaml") || strings.HasSuffix(parsed.Path, ".yml") {
		parsed.Path = pathDir(parsed.Path) + "/" + strings.TrimPrefix(asset, "/")
	} else {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.TrimPrefix(asset, "/")
	}
	return parsed.String(), nil
}

func pathDir(value string) string {
	value = strings.TrimSuffix(value, "/")
	idx := strings.LastIndex(value, "/")
	if idx < 0 {
		return ""
	}
	return value[:idx]
}

func (m *Market) readInstalled() ([]Item, error) {
	b, err := os.ReadFile(m.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []Item{}, nil
		}
		return nil, fmt.Errorf("read installed skills: %w", err)
	}
	var out []Item
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode installed skills: %w", err)
	}
	filtered := make([]Item, 0, len(out))
	for _, item := range out {
		if m.isBuiltinSkill(item.Name) {
			continue
		}
		filtered = append(filtered, item)
	}
	out = filtered
	sort.Slice(out, func(i, j int) bool { return out[i].InstalledAt.After(out[j].InstalledAt) })
	return out, nil
}

func (m *Market) writeInstalled(items []Item) error {
	if err := os.MkdirAll(filepath.Dir(m.storePath), 0o755); err != nil {
		return fmt.Errorf("create skills dir: %w", err)
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installed skills: %w", err)
	}
	if err := os.WriteFile(m.storePath, b, 0o644); err != nil {
		return fmt.Errorf("write installed skills: %w", err)
	}
	return nil
}

func (m *Market) localCatalog() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, len(m.catalog))
	copy(out, m.catalog)
	return out
}

func (m *Market) remoteCatalogWithCache() []Item {
	m.mu.Lock()
	url := strings.TrimSpace(m.remoteURL)
	cached := cloneItems(m.remoteCatalog)
	at := m.remoteCatalogAt
	m.mu.Unlock()

	if url == "" {
		return []Item{}
	}
	if len(cached) > 0 && time.Since(at) < remoteCacheTTL {
		return cached
	}

	items, err := m.fetchRemoteCatalog(url)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.remoteCatalogErr = err.Error()
		if len(m.remoteCatalog) == 0 {
			m.remoteCatalogAt = time.Now().UTC().Add(-remoteCacheTTL + 20*time.Second)
		}
		return cloneItems(m.remoteCatalog)
	}
	m.remoteCatalogErr = ""
	m.remoteCatalog = items
	m.remoteCatalogAt = time.Now().UTC()
	return cloneItems(items)
}

func (m *Market) fetchRemoteCatalog(url string) ([]Item, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build market request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("load remote market: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("load remote market: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read remote market: %w", err)
	}
	items, err := decodeRemoteCatalog(body)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Name = strings.TrimSpace(items[i].Name)
		items[i].Description = strings.TrimSpace(items[i].Description)
		items[i].Version = strings.TrimSpace(items[i].Version)
		items[i].Source = strings.TrimSpace(items[i].Source)
		items[i].SkillURL = strings.TrimSpace(items[i].SkillURL)
		if items[i].Version == "" {
			items[i].Version = "latest"
		}
		if items[i].Source == "" {
			items[i].Source = url
		}
	}
	trimmed := make([]Item, 0, len(items))
	for _, item := range items {
		if item.Name == "" {
			continue
		}
		trimmed = append(trimmed, item)
	}
	return trimmed, nil
}

func decodeRemoteCatalog(raw []byte) ([]Item, error) {
	var list []Item
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var payload struct {
		Items   []Item `json:"items"`
		Catalog []Item `json:"catalog"`
		Data    []Item `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode remote market catalog: %w", err)
	}
	switch {
	case len(payload.Items) > 0:
		return payload.Items, nil
	case len(payload.Catalog) > 0:
		return payload.Catalog, nil
	case len(payload.Data) > 0:
		return payload.Data, nil
	default:
		return []Item{}, nil
	}
}

func dedupeCatalog(items []Item) []Item {
	seen := make(map[string]struct{}, len(items))
	out := make([]Item, 0, len(items))
	for _, item := range items {
		key := strings.ToLower(strings.TrimSpace(item.Name)) + "@" + strings.ToLower(strings.TrimSpace(item.Version))
		if key == "@" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func filterCatalog(items []Item, query string) []Item {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, item := range items {
		text := strings.ToLower(strings.Join([]string{item.Name, item.Description, item.Source}, " "))
		if strings.Contains(text, q) {
			out = append(out, item)
		}
	}
	return out
}

func cloneItems(items []Item) []Item {
	out := make([]Item, len(items))
	copy(out, items)
	return out
}

func copyDirContents(srcDir, dstDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := writeSkillFile(dstDir, rel, content); err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("copy skill directory: %w", err)
	}
	return dedupeStrings(files), nil
}

func unzipBytes(body []byte, dstDir string) ([]string, error) {
	tmpFile, err := os.CreateTemp("", "xclaw-skill-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp zip: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)
	if _, err := tmpFile.Write(body); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write temp zip: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp zip: %w", err)
	}
	r, err := zip.OpenReader(tmpName)
	if err != nil {
		return nil, fmt.Errorf("open skill zip: %w", err)
	}
	defer r.Close()
	files := make([]string, 0, len(r.File))
	rootPrefix := zipSingleRootPrefix(r.File)
	for _, file := range r.File {
		cleanName := filepath.Clean(file.Name)
		if cleanName == "." || strings.HasPrefix(cleanName, "..") {
			continue
		}
		if rootPrefix != "" {
			cleanName = strings.TrimPrefix(filepath.ToSlash(cleanName), rootPrefix+"/")
			cleanName = filepath.Clean(cleanName)
			if cleanName == "." || cleanName == "" {
				continue
			}
		}
		target := filepath.Join(dstDir, cleanName)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, fmt.Errorf("create zip dir: %w", err)
			}
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry: %w", err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip entry: %w", err)
		}
		if err := writeSkillFile(dstDir, cleanName, content); err != nil {
			return nil, err
		}
		files = append(files, filepath.ToSlash(cleanName))
	}
	return dedupeStrings(files), nil
}

func zipSingleRootPrefix(files []*zip.File) string {
	root := ""
	for _, file := range files {
		cleanName := filepath.Clean(file.Name)
		if cleanName == "." || strings.HasPrefix(cleanName, "..") {
			continue
		}
		cleanName = filepath.ToSlash(cleanName)
		parts := strings.Split(cleanName, "/")
		if len(parts) <= 1 {
			return ""
		}
		if root == "" {
			root = parts[0]
			continue
		}
		if parts[0] != root {
			return ""
		}
	}
	return root
}

func writeSkillFile(skillDir, rel string, content []byte) error {
	rel = filepath.Clean(strings.TrimSpace(rel))
	if rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("invalid skill file path: %s", rel)
	}
	target := filepath.Join(skillDir, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create skill subdir: %w", err)
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return fmt.Errorf("write skill file %s: %w", rel, err)
	}
	return nil
}

func loadManifestFromDir(skillDir string) (*SkillPackageManifest, []string, error) {
	for _, name := range []string{"skill.json", "skill.yaml", "skill.yml"} {
		body, err := os.ReadFile(filepath.Join(skillDir, name))
		if err != nil {
			continue
		}
		manifest, _, ok := decodeManifestPayload(name, body)
		if ok {
			return &manifest, []string{name}, nil
		}
	}
	return nil, nil, nil
}

func computeDirIntegrity(root string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.Base(rel) == installLockName {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, rel := range files {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, rel)
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(content)
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func verifyChecksum(body []byte, expected string) error {
	expected = normalizeChecksum(expected)
	if expected == "" {
		return nil
	}
	sum := sha256.Sum256(body)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch: expected %s got %s", expected, actual)
	}
	return nil
}

func normalizeChecksum(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "sha256:") {
		return raw
	}
	return "sha256:" + raw
}

func manifestDependencies(manifest *SkillPackageManifest) []string {
	if manifest == nil {
		return nil
	}
	return manifest.Dependencies
}

func cloneStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	copy(out, items)
	return out
}

func dedupeStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(filepath.ToSlash(item))
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

type dependencySpec struct {
	Raw        string
	Name       string
	Version    string
	Constraint string
}

func parseDependencySpec(raw string) dependencySpec {
	raw = strings.TrimSpace(raw)
	spec := dependencySpec{
		Raw:  raw,
		Name: raw,
	}
	if raw == "" || looksLikeURL(raw) {
		return spec
	}
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at >= len(raw)-1 {
		return spec
	}
	name := strings.TrimSpace(raw[:at])
	req := strings.TrimSpace(raw[at+1:])
	if name == "" || req == "" {
		return spec
	}
	spec.Name = name
	spec.Raw = name
	if isExactVersion(req) {
		spec.Version = req
	} else {
		spec.Constraint = req
	}
	return spec
}

func requestedConstraint(opts InstallOptions) string {
	if strings.TrimSpace(opts.Constraint) != "" {
		return strings.TrimSpace(opts.Constraint)
	}
	return strings.TrimSpace(opts.Version)
}

func matchesVersionConstraint(version, constraint string) bool {
	version = strings.TrimSpace(version)
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return true
	}
	if version == "" {
		return false
	}
	if isExactVersion(constraint) {
		return compareVersionStrings(version, constraint) == 0
	}
	versionSem, ok := parseSemVersion(version)
	if !ok {
		return strings.EqualFold(version, constraint)
	}
	for _, part := range splitConstraintParts(constraint) {
		if !matchConstraintPart(versionSem, part) {
			return false
		}
	}
	return true
}

func splitConstraintParts(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func matchConstraintPart(version semVersion, raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	switch {
	case strings.HasPrefix(raw, "^"):
		base, ok := parseSemVersion(strings.TrimSpace(strings.TrimPrefix(raw, "^")))
		if !ok {
			return false
		}
		return matchCaretConstraint(version, base)
	case strings.HasPrefix(raw, "~"):
		base, ok := parseSemVersion(strings.TrimSpace(strings.TrimPrefix(raw, "~")))
		if !ok {
			return false
		}
		return compareSemVersion(version, base) >= 0 &&
			version.major == base.major &&
			version.minor == base.minor
	default:
		op, rhs := parseConstraintOperator(raw)
		target, ok := parseSemVersion(rhs)
		if !ok {
			return false
		}
		cmp := compareSemVersion(version, target)
		switch op {
		case "", "=", "==":
			return cmp == 0
		case "!=":
			return cmp != 0
		case ">":
			return cmp > 0
		case ">=":
			return cmp >= 0
		case "<":
			return cmp < 0
		case "<=":
			return cmp <= 0
		default:
			return false
		}
	}
}

func parseConstraintOperator(raw string) (string, string) {
	for _, op := range []string{">=", "<=", "!=", "==", ">", "<", "="} {
		if strings.HasPrefix(raw, op) {
			return op, strings.TrimSpace(strings.TrimPrefix(raw, op))
		}
	}
	return "", strings.TrimSpace(raw)
}

func matchCaretConstraint(version, base semVersion) bool {
	if compareSemVersion(version, base) < 0 {
		return false
	}
	switch {
	case base.major > 0:
		return version.major == base.major
	case base.minor > 0:
		return version.major == 0 && version.minor == base.minor
	default:
		return version.major == 0 && version.minor == 0 && version.patch == base.patch
	}
}

func isExactVersion(raw string) bool {
	_, ok := parseSemVersion(raw)
	return ok
}

func compareVersionStrings(left, right string) int {
	lv, lok := parseSemVersion(left)
	rv, rok := parseSemVersion(right)
	if lok && rok {
		return compareSemVersion(lv, rv)
	}
	return strings.Compare(strings.ToLower(strings.TrimSpace(left)), strings.ToLower(strings.TrimSpace(right)))
}

type semVersion struct {
	major int
	minor int
	patch int
	pre   string
}

func parseSemVersion(raw string) (semVersion, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(raw), "v"))
	if raw == "" {
		return semVersion{}, false
	}
	if idx := strings.Index(raw, "+"); idx >= 0 {
		raw = raw[:idx]
	}
	pre := ""
	if idx := strings.Index(raw, "-"); idx >= 0 {
		pre = raw[idx+1:]
		raw = raw[:idx]
	}
	parts := strings.Split(raw, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semVersion{}, false
	}
	ints := []int{0, 0, 0}
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return semVersion{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return semVersion{}, false
		}
		ints[i] = n
	}
	return semVersion{
		major: ints[0],
		minor: ints[1],
		patch: ints[2],
		pre:   pre,
	}, true
}

func compareSemVersion(left, right semVersion) int {
	switch {
	case left.major != right.major:
		return compareInts(left.major, right.major)
	case left.minor != right.minor:
		return compareInts(left.minor, right.minor)
	case left.patch != right.patch:
		return compareInts(left.patch, right.patch)
	case left.pre == right.pre:
		return 0
	case left.pre == "":
		return 1
	case right.pre == "":
		return -1
	default:
		return strings.Compare(left.pre, right.pre)
	}
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func baseNameFromSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if looksLikeURL(source) {
		parsed, err := url.Parse(source)
		if err == nil {
			return strings.TrimSuffix(path.Base(parsed.Path), filepath.Ext(parsed.Path))
		}
	}
	return strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
}

func looksLikeURL(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func slugifySkillName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
