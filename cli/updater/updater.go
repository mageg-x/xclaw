package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

func FetchLatest(ctx context.Context, client *http.Client, repo string) (Release, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return Release{}, fmt.Errorf("release repo is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return Release{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, err
	}
	return release, nil
}

func SelectPlatformAsset(release Release, goos, goarch string) (Asset, *Asset, error) {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	checksum := findChecksumAsset(release.Assets)
	bestScore := -1
	best := Asset{}
	for _, asset := range release.Assets {
		score := scorePlatformAsset(asset.Name, goos, goarch)
		if score > bestScore {
			bestScore = score
			best = asset
		}
	}
	if bestScore < 0 {
		return Asset{}, checksum, fmt.Errorf("no release asset found for %s/%s", goos, goarch)
	}
	return best, checksum, nil
}

func DownloadAndExtractBinary(ctx context.Context, client *http.Client, asset Asset, checksum *Asset, workDir string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	archivePath := filepath.Join(workDir, asset.Name)
	if err := downloadFile(ctx, client, asset.DownloadURL, archivePath); err != nil {
		return "", err
	}
	if checksum != nil && strings.TrimSpace(checksum.DownloadURL) != "" {
		checksumPath := filepath.Join(workDir, checksum.Name)
		if err := downloadFile(ctx, client, checksum.DownloadURL, checksumPath); err == nil {
			if err := verifyChecksumFile(archivePath, checksumPath, asset.Name); err != nil {
				return "", err
			}
		}
	}
	binaryPath, err := extractBinary(archivePath, workDir)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(binaryPath, 0o755); err != nil && runtime.GOOS != "windows" {
		return "", err
	}
	return binaryPath, nil
}

func downloadFile(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("download %s failed: status=%d body=%s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

func verifyChecksumFile(filePath, checksumPath, assetName string) error {
	raw, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	expected, err := findChecksumEntry(string(raw), assetName)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s got %s", assetName, expected, actual)
	}
	return nil
}

func findChecksumEntry(raw, assetName string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum entry not found for %s", assetName)
}

func extractBinary(archivePath, workDir string) (string, error) {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZipBinary(archivePath, workDir)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGzBinary(archivePath, workDir)
	default:
		return archivePath, nil
	}
}

func extractZipBinary(archivePath, workDir string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || !isAgentBinary(file.Name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		target := filepath.Join(workDir, filepath.Base(file.Name))
		out, err := os.Create(target)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", fmt.Errorf("agent binary not found in %s", filepath.Base(archivePath))
}

func extractTarGzBinary(archivePath, workDir string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if header.FileInfo().IsDir() || !isAgentBinary(header.Name) {
			continue
		}
		target := filepath.Join(workDir, filepath.Base(header.Name))
		out, err := os.Create(target)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, reader); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", fmt.Errorf("agent binary not found in %s", filepath.Base(archivePath))
}

func isAgentBinary(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == "agent" || base == "agent.exe"
}

func findChecksumAsset(assets []Asset) *Asset {
	for i := range assets {
		name := strings.ToLower(strings.TrimSpace(assets[i].Name))
		if strings.Contains(name, "checksum") {
			return &assets[i]
		}
	}
	return nil
}

func scorePlatformAsset(name, goos, goarch string) int {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || strings.Contains(lower, "checksum") {
		return -1
	}
	score := 0
	if strings.Contains(lower, goos) {
		score += 10
	} else {
		return -1
	}
	for _, alias := range archAliases(goarch) {
		if strings.Contains(lower, alias) {
			score += 10
			break
		}
	}
	if score < 20 {
		return -1
	}
	switch {
	case strings.HasSuffix(lower, ".zip"):
		score += 2
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		score += 2
	}
	return score
}

func archAliases(goarch string) []string {
	switch goarch {
	case "amd64":
		return []string{"amd64", "x86_64"}
	case "arm64":
		return []string{"arm64", "aarch64"}
	default:
		return []string{goarch}
	}
}
