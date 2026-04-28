package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractDocumentTextOfficeAndArchives(t *testing.T) {
	t.Run("docx", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.docx")
		writeZipFixture(t, path, map[string]string{
			"word/document.xml": `<w:document><w:body><w:p><w:r><w:t>Hello DOCX</w:t></w:r></w:p></w:body></w:document>`,
		})
		text, err := extractDocumentText(path, "")
		if err != nil {
			t.Fatalf("extract docx: %v", err)
		}
		if !strings.Contains(text, "Hello DOCX") {
			t.Fatalf("unexpected docx text: %q", text)
		}
	})

	t.Run("xlsx", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sheet.xlsx")
		writeZipFixture(t, path, map[string]string{
			"xl/sharedStrings.xml":     `<sst><si><t>Name</t></si><si><t>Alice</t></si></sst>`,
			"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c><c><v>42</v></c></row></sheetData></worksheet>`,
		})
		text, err := extractDocumentText(path, "")
		if err != nil {
			t.Fatalf("extract xlsx: %v", err)
		}
		for _, want := range []string{"Name", "Alice", "42"} {
			if !strings.Contains(text, want) {
				t.Fatalf("xlsx text missing %q: %q", want, text)
			}
		}
	})

	t.Run("pptx", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "slides.pptx")
		writeZipFixture(t, path, map[string]string{
			"ppt/slides/slide1.xml": `<p:sld><a:t>Intro Slide</a:t><a:t>Roadmap</a:t></p:sld>`,
		})
		text, err := extractDocumentText(path, "")
		if err != nil {
			t.Fatalf("extract pptx: %v", err)
		}
		for _, want := range []string{"Intro Slide", "Roadmap"} {
			if !strings.Contains(text, want) {
				t.Fatalf("pptx text missing %q: %q", want, text)
			}
		}
	})

	t.Run("zip archive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "code.zip")
		writeZipFixture(t, path, map[string]string{
			"src/main.go":     "package main\n\nfunc main() {}\n",
			"README.md":       "# Demo\nzip archive fixture\n",
			"assets/logo.png": "PNG",
		})
		text, err := extractDocumentText(path, "")
		if err != nil {
			t.Fatalf("extract zip: %v", err)
		}
		for _, want := range []string{"src/main.go", "README.md", "package main", "zip archive fixture"} {
			if !strings.Contains(text, want) {
				t.Fatalf("zip text missing %q: %q", want, text)
			}
		}
	})

	t.Run("tar.gz archive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "code.tar.gz")
		writeTarGzFixture(t, path, map[string]string{
			"pkg/app.ts":  "export const app = 'xclaw'\n",
			"docs/USE.md": "tar gz fixture\n",
		})
		text, err := extractDocumentText(path, "")
		if err != nil {
			t.Fatalf("extract tar.gz: %v", err)
		}
		for _, want := range []string{"pkg/app.ts", "docs/USE.md", "xclaw", "tar gz fixture"} {
			if !strings.Contains(text, want) {
				t.Fatalf("tar.gz text missing %q: %q", want, text)
			}
		}
	})
}

func TestAnalyzeDocumentWithOpenAIUsesResponsesFileInput(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(tmp, []byte("%PDF-1.4\nfixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := openAIHTTPClient
	t.Cleanup(func() { openAIHTTPClient = prev })
	openAIHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://api.openai.com/v1/responses" {
				t.Fatalf("unexpected url: %s", req.URL.String())
			}
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			input, _ := payload["input"].([]any)
			if len(input) != 1 {
				t.Fatalf("unexpected input: %#v", payload["input"])
			}
			msg, _ := input[0].(map[string]any)
			content, _ := msg["content"].([]any)
			if len(content) != 2 {
				t.Fatalf("unexpected content: %#v", msg["content"])
			}
			filePart, _ := content[1].(map[string]any)
			if filePart["type"] != "input_file" {
				t.Fatalf("unexpected file part: %#v", filePart)
			}
			if !strings.HasPrefix(filePart["file_data"].(string), "data:application/pdf;base64,") {
				t.Fatalf("unexpected file_data: %v", filePart["file_data"])
			}
			respBody := `{"output":[{"content":[{"type":"output_text","text":"cloud parsed text"}]}]}`
			return jsonHTTPResponse(http.StatusOK, []byte(respBody)), nil
		}),
	}

	text, err := analyzeDocumentWithOpenAI(context.Background(), "sk-test", "gpt-4.1-mini", "summarize", tmp, "application/pdf")
	if err != nil {
		t.Fatalf("analyze document: %v", err)
	}
	if text != "cloud parsed text" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestScanFileForViruses(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(tmp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("disabled", func(t *testing.T) {
		t.Setenv("XCLAW_VIRUS_SCAN_URL", "")
		result, err := scanFileForViruses(context.Background(), tmp, "text/plain")
		if err != nil {
			t.Fatalf("scan disabled: %v", err)
		}
		if result.Status != "skipped" {
			t.Fatalf("unexpected status: %#v", result)
		}
	})

	t.Run("clean", func(t *testing.T) {
		t.Setenv("XCLAW_VIRUS_SCAN_URL", "https://scanner.example/scan")
		prev := virusScanHTTPClient
		t.Cleanup(func() { virusScanHTTPClient = prev })
		virusScanHTTPClient = &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://scanner.example/scan" {
					t.Fatalf("unexpected url: %s", req.URL.String())
				}
				respBody := `{"clean":true,"status":"clean"}`
				return jsonHTTPResponse(http.StatusOK, []byte(respBody)), nil
			}),
		}
		result, err := scanFileForViruses(context.Background(), tmp, "text/plain")
		if err != nil {
			t.Fatalf("scan clean: %v", err)
		}
		if result.Status != "clean" {
			t.Fatalf("unexpected result: %#v", result)
		}
	})

	t.Run("infected", func(t *testing.T) {
		t.Setenv("XCLAW_VIRUS_SCAN_URL", "https://scanner.example/scan")
		prev := virusScanHTTPClient
		t.Cleanup(func() { virusScanHTTPClient = prev })
		virusScanHTTPClient = &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				respBody := `{"clean":false,"status":"infected","detail":"eicar"}`
				return jsonHTTPResponse(http.StatusOK, []byte(respBody)), nil
			}),
		}
		_, err := scanFileForViruses(context.Background(), tmp, "text/plain")
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("expected infected rejection, got %v", err)
		}
	})
}

func writeZipFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	fd, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer fd.Close()

	zw := zip.NewWriter(fd)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

func writeTarGzFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	fd, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tar.gz: %v", err)
	}
	defer fd.Close()

	gzw := gzip.NewWriter(fd)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		body := []byte(content)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonHTTPResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
