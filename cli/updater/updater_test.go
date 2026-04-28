package updater

import "testing"

func TestSelectPlatformAsset(t *testing.T) {
	release := Release{
		TagName: "v1.2.3",
		Assets: []Asset{
			{Name: "checksums.txt", DownloadURL: "https://example/checksums.txt"},
			{Name: "agent_linux_arm64.tar.gz", DownloadURL: "https://example/agent_linux_arm64.tar.gz"},
			{Name: "agent_linux_amd64.tar.gz", DownloadURL: "https://example/agent_linux_amd64.tar.gz"},
			{Name: "agent_windows_amd64.zip", DownloadURL: "https://example/agent_windows_amd64.zip"},
		},
	}

	asset, checksum, err := SelectPlatformAsset(release, "linux", "amd64")
	if err != nil {
		t.Fatalf("SelectPlatformAsset failed: %v", err)
	}
	if asset.Name != "agent_linux_amd64.tar.gz" {
		t.Fatalf("unexpected asset: %s", asset.Name)
	}
	if checksum == nil || checksum.Name != "checksums.txt" {
		t.Fatalf("expected checksum asset, got %+v", checksum)
	}
}

func TestFindChecksumEntry(t *testing.T) {
	raw := `
abc123  agent_linux_amd64.tar.gz
def456 *agent_windows_amd64.zip
`
	got, err := findChecksumEntry(raw, "agent_windows_amd64.zip")
	if err != nil {
		t.Fatalf("findChecksumEntry failed: %v", err)
	}
	if got != "def456" {
		t.Fatalf("unexpected checksum: %s", got)
	}
}

func TestScorePlatformAsset(t *testing.T) {
	if got := scorePlatformAsset("checksums.txt", "linux", "amd64"); got != -1 {
		t.Fatalf("expected checksum asset to be ignored, got %d", got)
	}
	if got := scorePlatformAsset("agent_linux_amd64.tar.gz", "linux", "amd64"); got < 20 {
		t.Fatalf("expected valid platform score, got %d", got)
	}
	if got := scorePlatformAsset("agent_darwin_arm64.tar.gz", "linux", "amd64"); got != -1 {
		t.Fatalf("expected mismatched platform score -1, got %d", got)
	}
}
