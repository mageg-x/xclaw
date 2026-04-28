package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"xclaw/cli/engine"
	"xclaw/cli/llm"
)

const (
	knowledgeMountSettingKey = "knowledge_mounts_json"
	modelRoutingSettingKey   = "model_routing_json"
)

var (
	openAIHTTPClient    = http.DefaultClient
	virusScanHTTPClient = http.DefaultClient
)

type knowledgeMount struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	Source        string    `json:"source"`
	Token         string    `json:"token,omitempty"`
	Status        string    `json:"status"`
	LastError     string    `json:"last_error,omitempty"`
	IndexedChunks int       `json:"indexed_chunks"`
	LastIndexedAt time.Time `json:"last_indexed_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Server) loadModelRouting() {
	if s.localGateway == nil {
		return
	}
	raw, ok, err := s.store.GetSetting(context.Background(), modelRoutingSettingKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var cfg llm.RoutingConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return
	}
	s.localGateway.SetRoutingConfig(cfg)
}

func (s *Server) handleModelRouting(w http.ResponseWriter, r *http.Request) {
	if s.localGateway == nil {
		writeError(w, http.StatusNotImplemented, errText("local gateway not available"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.localGateway.GetRoutingConfig())
	case http.MethodPut:
		var cfg llm.RoutingConfig
		if err := decodeJSON(r, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.localGateway.SetRoutingConfig(cfg)
		raw, _ := json.Marshal(cfg)
		_ = s.store.SetSetting(r.Context(), modelRoutingSettingKey, string(raw))
		writeJSON(w, http.StatusOK, s.localGateway.GetRoutingConfig())
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleKnowledgeMounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.loadKnowledgeMounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var req knowledgeMount
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.Type = strings.ToLower(strings.TrimSpace(req.Type))
		req.Source = strings.TrimSpace(req.Source)
		if req.Name == "" || req.Type == "" || req.Source == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("name/type/source required"))
			return
		}
		if req.ID == "" {
			req.ID = engine.NewID("kb")
		}
		req.Status = "indexing"
		req.UpdatedAt = time.Now().UTC()

		items, err := s.loadKnowledgeMounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		updated := false
		for i := range items {
			if items[i].ID == req.ID {
				items[i] = req
				updated = true
				break
			}
		}
		if !updated {
			items = append(items, req)
		}
		if err := s.saveKnowledgeMounts(r.Context(), items); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		count, err := s.indexKnowledgeMount(r.Context(), &req)
		req.IndexedChunks = count
		req.LastIndexedAt = time.Now().UTC()
		req.UpdatedAt = req.LastIndexedAt
		if err != nil {
			req.Status = "error"
			req.LastError = err.Error()
		} else {
			req.Status = "ready"
			req.LastError = ""
		}
		for i := range items {
			if items[i].ID == req.ID {
				items[i] = req
			}
		}
		_ = s.saveKnowledgeMounts(r.Context(), items)
		writeJSON(w, http.StatusOK, req)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleKnowledgeReindex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id required"))
		return
	}
	items, err := s.loadKnowledgeMounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for i := range items {
		if items[i].ID != id {
			continue
		}
		items[i].Status = "indexing"
		items[i].UpdatedAt = time.Now().UTC()
		_ = s.saveKnowledgeMounts(r.Context(), items)
		count, indexErr := s.indexKnowledgeMount(r.Context(), &items[i])
		items[i].IndexedChunks = count
		items[i].LastIndexedAt = time.Now().UTC()
		items[i].UpdatedAt = items[i].LastIndexedAt
		if indexErr != nil {
			items[i].Status = "error"
			items[i].LastError = indexErr.Error()
		} else {
			items[i].Status = "ready"
			items[i].LastError = ""
		}
		_ = s.saveKnowledgeMounts(r.Context(), items)
		writeJSON(w, http.StatusOK, items[i])
		return
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("mount not found"))
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		MountID string `json:"mount_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query required"))
		return
	}
	if req.Limit <= 0 {
		req.Limit = 8
	}
	hits, err := s.store.SearchVectorMemory(r.Context(), buildHashEmbeddingForAPI(req.Query), req.Limit*5, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	filtered := make([]any, 0, req.Limit)
	prefix := "kb:"
	if strings.TrimSpace(req.MountID) != "" {
		prefix = "kb:" + strings.TrimSpace(req.MountID)
	}
	for _, hit := range hits {
		if !strings.HasPrefix(hit.AgentID, "kb:") {
			continue
		}
		if !strings.HasPrefix(hit.AgentID, prefix) {
			continue
		}
		filtered = append(filtered, hit)
		if len(filtered) >= req.Limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (s *Server) handleMultimodalAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID       string `json:"id"`
		Prompt   string `json:"prompt"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	files, err := s.loadMultimodalFiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var target map[string]any
	for _, item := range files {
		if strings.TrimSpace(fmt.Sprintf("%v", item["id"])) == strings.TrimSpace(req.ID) {
			target = item
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("file not found"))
		return
	}
	path := strings.TrimSpace(fmt.Sprintf("%v", target["path"]))
	mime := strings.TrimSpace(fmt.Sprintf("%v", target["mime"]))
	if req.Prompt == "" {
		req.Prompt = "请提取文件的关键内容并给出结构化摘要"
	}

	if strings.HasPrefix(mime, "image/") {
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		}
		if apiKey != "" {
			model := firstText(req.Model, "gpt-4o-mini")
			summary, callErr := analyzeImageWithOpenAI(r.Context(), apiKey, model, req.Prompt, path, mime)
			if callErr == nil {
				writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "mode": "vision-api"})
				return
			}
		}
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey != "" {
		model := firstText(req.Model, "gpt-4.1-mini")
		summary, callErr := analyzeDocumentWithOpenAI(r.Context(), apiKey, model, req.Prompt, path, mime)
		if callErr == nil {
			writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "mode": "document-api"})
			return
		}
	}

	text, extErr := extractDocumentText(path, mime)
	if extErr != nil {
		writeError(w, http.StatusBadRequest, extErr)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "未提取到可读文本，建议改用视觉模型。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": trimString(text, 3000),
		"mode":    "local-extractor",
	})
}

func (s *Server) handleMultimodalRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	title := firstText(req.Title, "Generated")
	prompt := strings.TrimSpace(req.Prompt)
	switch kind {
	case "mermaid":
		lines := splitNonEmptyLines(prompt)
		if len(lines) == 0 {
			lines = []string{"Start", "Analyze", "Deliver"}
		}
		var b strings.Builder
		b.WriteString("flowchart TD\n")
		for i := 0; i < len(lines)-1; i++ {
			b.WriteString(fmt.Sprintf("    N%d[%s] --> N%d[%s]\n", i, sanitizeMermaid(lines[i]), i+1, sanitizeMermaid(lines[i+1])))
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "mermaid", "content": b.String(), "title": title})
	case "echarts":
		nums := extractNumbers(prompt)
		if len(nums) == 0 {
			nums = []float64{12, 18, 9, 22}
		}
		labels := make([]string, len(nums))
		for i := range nums {
			labels[i] = fmt.Sprintf("P%d", i+1)
		}
		option := map[string]any{
			"title":   map[string]any{"text": title},
			"tooltip": map[string]any{},
			"xAxis":   map[string]any{"type": "category", "data": labels},
			"yAxis":   map[string]any{"type": "value"},
			"series":  []map[string]any{{"type": "bar", "data": nums}},
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "echarts", "content": option, "title": title})
	case "html":
		html := "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + title + "</title></head><body><main><h1>" + title + "</h1><pre>" + escapeHTML(prompt) + "</pre></main></body></html>"
		writeJSON(w, http.StatusOK, map[string]any{"kind": "html", "content": html, "title": title})
	case "image":
		apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if apiKey == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("OPENAI_API_KEY required for image rendering"))
			return
		}
		image, err := renderImageWithOpenAI(r.Context(), apiKey, "gpt-image-1", prompt)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "image", "content": image, "title": title})
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported kind: %s", kind))
	}
}

func (s *Server) handleAudioSTT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("api_key required"))
		return
	}
	model := firstText(r.FormValue("model"), "whisper-1")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	buf := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(buf)
	_ = writer.WriteField("model", model)
	part, err := writer.CreateFormFile("file", header.Filename)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(part, file); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = writer.Close()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://api.openai.com/v1/audio/transcriptions", buf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("stt upstream: %s", string(raw)))
		return
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	writeJSON(w, http.StatusOK, map[string]any{"text": fmt.Sprintf("%v", parsed["text"])})
}

func (s *Server) handleAudioTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var reqBody struct {
		Text   string `json:"text"`
		Voice  string `json:"voice"`
		Model  string `json:"model"`
		APIKey string `json:"api_key"`
	}
	if err := decodeJSON(r, &reqBody); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(reqBody.Text) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("text required"))
		return
	}
	apiKey := strings.TrimSpace(reqBody.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("api_key required"))
		return
	}
	voice := firstText(reqBody.Voice, "alloy")
	model := firstText(reqBody.Model, "gpt-4o-mini-tts")
	payload := map[string]any{
		"model": model,
		"voice": voice,
		"input": reqBody.Text,
	}
	rawPayload, _ := json.Marshal(payload)
	upReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://api.openai.com/v1/audio/speech", bytes.NewReader(rawPayload))
	upReq.Header.Set("Authorization", "Bearer "+apiKey)
	upReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	audio, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("tts upstream: %s", string(audio)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mime":         firstText(resp.Header.Get("Content-Type"), "audio/mpeg"),
		"audio_base64": base64.StdEncoding.EncodeToString(audio),
		"bytes":        len(audio),
	})
}

func (s *Server) loadKnowledgeMounts(ctx context.Context) ([]knowledgeMount, error) {
	raw, ok, err := s.store.GetSetting(ctx, knowledgeMountSettingKey)
	if err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return []knowledgeMount{}, nil
	}
	var items []knowledgeMount
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) saveKnowledgeMounts(ctx context.Context, items []knowledgeMount) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, knowledgeMountSettingKey, string(raw))
}

func (s *Server) indexKnowledgeMount(ctx context.Context, mount *knowledgeMount) (int, error) {
	chunks := make([]string, 0, 256)
	switch mount.Type {
	case "local":
		localChunks, err := collectLocalChunks(mount.Source)
		if err != nil {
			return 0, err
		}
		chunks = append(chunks, localChunks...)
	case "git":
		root := filepath.Join(s.cfg.DataDir, "knowledge", "git", mount.ID)
		if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
			return 0, err
		}
		if _, err := os.Stat(root); err == nil {
			cmd := exec.CommandContext(ctx, "git", "-C", root, "pull", "--ff-only")
			if out, pullErr := cmd.CombinedOutput(); pullErr != nil {
				return 0, fmt.Errorf("git pull: %s", string(out))
			}
		} else {
			cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", mount.Source, root)
			if out, cloneErr := cmd.CombinedOutput(); cloneErr != nil {
				return 0, fmt.Errorf("git clone: %s", string(out))
			}
		}
		localChunks, err := collectLocalChunks(root)
		if err != nil {
			return 0, err
		}
		chunks = append(chunks, localChunks...)
	case "web":
		urls := splitByCommaOrLine(mount.Source)
		for _, u := range urls {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			if resp.StatusCode >= 400 {
				continue
			}
			text := stripHTML(string(raw))
			chunks = append(chunks, chunkText(text, 800)...)
		}
	case "notion":
		token := strings.TrimSpace(mount.Token)
		if token == "" {
			token = strings.TrimSpace(os.Getenv("NOTION_API_KEY"))
		}
		if token == "" {
			return 0, fmt.Errorf("notion token required")
		}
		notionChunks, err := fetchNotionChunks(ctx, token, mount.Source)
		if err != nil {
			return 0, err
		}
		chunks = append(chunks, notionChunks...)
	default:
		return 0, fmt.Errorf("unsupported mount type: %s", mount.Type)
	}

	if len(chunks) == 0 {
		return 0, fmt.Errorf("no extractable content from mount")
	}
	if len(chunks) > 300 {
		chunks = chunks[:300]
	}
	count := 0
	agentID := "kb:" + mount.ID
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		_, err := s.store.AddVectorMemory(ctx, agentID, "knowledge", trimString(chunk, 1600), buildHashEmbeddingForAPI(chunk))
		if err != nil {
			continue
		}
		count++
	}
	if count == 0 {
		return 0, fmt.Errorf("indexing failed")
	}
	return count, nil
}

func collectLocalChunks(root string) ([]string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("root required")
	}
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	allowed := map[string]struct{}{
		".md": {}, ".txt": {}, ".go": {}, ".js": {}, ".ts": {}, ".tsx": {}, ".py": {},
		".json": {}, ".yaml": {}, ".yml": {}, ".sql": {}, ".html": {}, ".css": {}, ".xml": {},
		".pdf": {}, ".docx": {}, ".xlsx": {}, ".pptx": {}, ".csv": {}, ".zip": {}, ".tgz": {},
	}
	chunks := make([]string, 0, 256)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		ext := normalizedDocumentExt(path)
		if _, ok := allowed[ext]; !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil
		}
		text, err := extractDocumentText(path, "")
		if err != nil {
			return nil
		}
		chunks = append(chunks, chunkText(text, 800)...)
		return nil
	})
	return chunks, nil
}

func extractDocumentText(path, mime string) (string, error) {
	ext := normalizedDocumentExt(path)
	if strings.HasPrefix(mime, "text/") || ext == ".md" || ext == ".txt" || ext == ".go" || ext == ".js" || ext == ".ts" || ext == ".tsx" || ext == ".py" || ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".sql" || ext == ".html" || ext == ".xml" || ext == ".css" || ext == ".csv" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	if ext == ".pdf" {
		if _, err := exec.LookPath("pdftotext"); err != nil {
			return "", fmt.Errorf("pdftotext not found")
		}
		cmd := exec.Command("pdftotext", path, "-")
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	if ext == ".docx" {
		return extractDOCXText(path)
	}
	if ext == ".xlsx" {
		return extractXLSXText(path)
	}
	if ext == ".pptx" {
		return extractPPTXText(path)
	}
	if ext == ".zip" {
		return extractZipArchiveText(path)
	}
	if ext == ".tar.gz" || ext == ".tgz" {
		return extractTarGzArchiveText(path)
	}
	return "", fmt.Errorf("unsupported document type")
}

func normalizedDocumentExt(path string) string {
	lower := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(lower, ".tgz"):
		return ".tgz"
	default:
		return strings.ToLower(filepath.Ext(lower))
	}
}

func extractDOCXText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	var parts []string
	for _, f := range r.File {
		if f.Name != "word/document.xml" && !strings.HasPrefix(f.Name, "word/header") && !strings.HasPrefix(f.Name, "word/footer") {
			continue
		}
		raw, err := readZipFileLimited(f, 2<<20)
		if err != nil {
			continue
		}
		text := stripXMLTags(string(raw))
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("docx has no extractable text")
	}
	return strings.Join(parts, "\n\n"), nil
}

func extractXLSXText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	sharedStrings := []string{}
	sheetFiles := make([]*zip.File, 0, 8)
	for _, f := range r.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			raw, err := readZipFileLimited(f, 2<<20)
			if err != nil {
				continue
			}
			sharedStrings = extractXMLTextNodes(string(raw))
		case strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml"):
			sheetFiles = append(sheetFiles, f)
		}
	}
	sheets := make([]string, 0, len(sheetFiles))
	for _, f := range sheetFiles {
		raw, err := readZipFileLimited(f, 2<<20)
		if err != nil {
			continue
		}
		sheetText := extractSpreadsheetSheetText(string(raw), sharedStrings)
		if strings.TrimSpace(sheetText) != "" {
			sheets = append(sheets, fmt.Sprintf("[%s]\n%s", filepath.Base(f.Name), sheetText))
		}
	}
	if len(sheets) == 0 && len(sharedStrings) > 0 {
		return strings.Join(sharedStrings, "\n"), nil
	}
	if len(sheets) == 0 {
		return "", fmt.Errorf("xlsx has no extractable text")
	}
	return strings.Join(sheets, "\n\n"), nil
}

func extractPPTXText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	parts := make([]string, 0, 16)
	for _, f := range r.File {
		if !(strings.HasPrefix(f.Name, "ppt/slides/slide") || strings.HasPrefix(f.Name, "ppt/notesSlides/notesSlide")) || !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		raw, err := readZipFileLimited(f, 2<<20)
		if err != nil {
			continue
		}
		text := strings.Join(extractXMLTextNodes(string(raw)), "\n")
		text = collapseSpaces(text)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, fmt.Sprintf("[%s]\n%s", filepath.Base(f.Name), text))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("pptx has no extractable text")
	}
	return strings.Join(parts, "\n\n"), nil
}

func extractZipArchiveText(path string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	parts := make([]string, 0, 32)
	total := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() || !isArchiveTextCandidate(f.Name) {
			continue
		}
		raw, err := readZipFileLimited(f, 1<<20)
		if err != nil {
			continue
		}
		text := collapseSpaces(string(raw))
		if strings.TrimSpace(text) == "" {
			continue
		}
		part := fmt.Sprintf("## %s\n%s", f.Name, trimString(text, 4000))
		total += len(part)
		if total > 120000 {
			break
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("archive has no extractable text files")
	}
	return strings.Join(parts, "\n\n"), nil
}

func extractTarGzArchiveText(path string) (string, error) {
	fd, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	gz, err := gzip.NewReader(fd)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	parts := make([]string, 0, 32)
	total := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr == nil || hdr.FileInfo().IsDir() || !isArchiveTextCandidate(hdr.Name) || hdr.Size > 1<<20 {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			continue
		}
		text := collapseSpaces(string(raw))
		if strings.TrimSpace(text) == "" {
			continue
		}
		part := fmt.Sprintf("## %s\n%s", hdr.Name, trimString(text, 4000))
		total += len(part)
		if total > 120000 {
			break
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("archive has no extractable text files")
	}
	return strings.Join(parts, "\n\n"), nil
}

func readZipFileLimited(f *zip.File, limit int64) ([]byte, error) {
	fd, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	return io.ReadAll(io.LimitReader(fd, limit))
}

func extractXMLTextNodes(raw string) []string {
	re := regexp.MustCompile(`<(?:[\w-]+:)?t[^>]*>(.*?)</(?:[\w-]+:)?t>`)
	matches := re.FindAllStringSubmatch(raw, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		text := collapseSpaces(html.UnescapeString(stripXMLTags(m[1])))
		if strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

func extractSpreadsheetSheetText(raw string, sharedStrings []string) string {
	cellRe := regexp.MustCompile(`<c\b([^>]*)>(.*?)</c>`)
	typeRe := regexp.MustCompile(`t="([^"]+)"`)
	valueRe := regexp.MustCompile(`<v>(.*?)</v>`)
	inlineRe := regexp.MustCompile(`<(?:[\w-]+:)?t[^>]*>(.*?)</(?:[\w-]+:)?t>`)
	matches := cellRe.FindAllStringSubmatch(raw, -1)
	values := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		cellType := ""
		if tm := typeRe.FindStringSubmatch(m[1]); len(tm) > 1 {
			cellType = tm[1]
		}
		body := m[2]
		switch cellType {
		case "s":
			vm := valueRe.FindStringSubmatch(body)
			if len(vm) < 2 {
				continue
			}
			idx, err := strconv.Atoi(strings.TrimSpace(vm[1]))
			if err != nil || idx < 0 || idx >= len(sharedStrings) {
				continue
			}
			values = append(values, sharedStrings[idx])
		case "inlineStr":
			for _, tm := range inlineRe.FindAllStringSubmatch(body, -1) {
				if len(tm) < 2 {
					continue
				}
				text := collapseSpaces(html.UnescapeString(stripXMLTags(tm[1])))
				if strings.TrimSpace(text) != "" {
					values = append(values, text)
				}
			}
		default:
			vm := valueRe.FindStringSubmatch(body)
			if len(vm) < 2 {
				continue
			}
			text := collapseSpaces(html.UnescapeString(stripXMLTags(vm[1])))
			if strings.TrimSpace(text) != "" {
				values = append(values, text)
			}
		}
	}
	return strings.Join(values, "\n")
}

func isArchiveTextCandidate(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if strings.HasPrefix(base, ".") {
		return false
	}
	ext := normalizedDocumentExt(base)
	switch ext {
	case ".md", ".txt", ".go", ".js", ".ts", ".tsx", ".jsx", ".py", ".java", ".rb", ".rs", ".php", ".sh",
		".json", ".yaml", ".yml", ".sql", ".html", ".css", ".xml", ".csv", ".toml":
		return true
	default:
		return false
	}
}

type virusScanResult struct {
	Mode   string `json:"mode"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func scanFileForViruses(ctx context.Context, path, mime string) (virusScanResult, error) {
	scanURL := strings.TrimSpace(os.Getenv("XCLAW_VIRUS_SCAN_URL"))
	if scanURL == "" {
		return virusScanResult{Mode: "disabled", Status: "skipped"}, nil
	}

	fd, err := os.Open(path)
	if err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}
	defer fd.Close()

	body := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("mime", strings.TrimSpace(mime))
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}
	if _, err := io.Copy(part, fd); err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}
	if err := writer.Close(); err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scanURL, body)
	if err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token := strings.TrimSpace(os.Getenv("XCLAW_VIRUS_SCAN_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := virusScanHTTPClient.Do(req)
	if err != nil {
		return virusScanResult{Mode: "http-api", Status: "error"}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return virusScanResult{Mode: "http-api", Status: "error", Detail: strings.TrimSpace(string(raw))}, fmt.Errorf("virus scan api status=%d", resp.StatusCode)
	}

	var payload struct {
		Clean   *bool  `json:"clean"`
		Status  string `json:"status"`
		Detail  string `json:"detail"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		status := strings.ToLower(strings.TrimSpace(firstText(payload.Status, payload.Verdict)))
		switch {
		case payload.Clean != nil && *payload.Clean:
			return virusScanResult{Mode: "http-api", Status: "clean", Detail: strings.TrimSpace(payload.Detail)}, nil
		case payload.Clean != nil && !*payload.Clean:
			return virusScanResult{Mode: "http-api", Status: firstText(status, "infected"), Detail: strings.TrimSpace(payload.Detail)}, fmt.Errorf("virus scan rejected file: %s", firstText(payload.Detail, "infected"))
		case status == "clean" || status == "ok" || status == "passed":
			return virusScanResult{Mode: "http-api", Status: "clean", Detail: strings.TrimSpace(payload.Detail)}, nil
		case status == "infected" || status == "blocked" || status == "malicious":
			return virusScanResult{Mode: "http-api", Status: status, Detail: strings.TrimSpace(payload.Detail)}, fmt.Errorf("virus scan rejected file: %s", firstText(payload.Detail, status))
		}
	}

	text := strings.ToLower(strings.TrimSpace(string(raw)))
	switch text {
	case "", "ok", "clean", "passed":
		return virusScanResult{Mode: "http-api", Status: "clean"}, nil
	default:
		return virusScanResult{Mode: "http-api", Status: "clean", Detail: trimString(strings.TrimSpace(string(raw)), 240)}, nil
	}
}

func renderImageWithOpenAI(ctx context.Context, apiKey, model, prompt string) (map[string]any, error) {
	payload := map[string]any{
		"model":  firstText(model, "gpt-image-1"),
		"prompt": firstText(prompt, "请生成一张简洁的说明性插图"),
		"size":   "1024x1024",
	}
	rawPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/images/generations", bytes.NewReader(rawPayload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := openAIHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("image api error: %s", string(body))
	}
	var result struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("empty image response")
	}
	item := result.Data[0]
	url := strings.TrimSpace(item.URL)
	mime := "image/png"
	if strings.TrimSpace(item.B64JSON) != "" {
		url = "data:" + mime + ";base64," + item.B64JSON
	}
	if url == "" {
		return nil, fmt.Errorf("image response missing content")
	}
	return map[string]any{
		"url":            url,
		"mime":           mime,
		"revised_prompt": item.RevisedPrompt,
	}, nil
}

func analyzeImageWithOpenAI(ctx context.Context, apiKey, model, prompt, path, mime string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > 5<<20 {
		return "", fmt.Errorf("image too large for inline vision request")
	}
	dataURI := "data:" + firstText(mime, "image/png") + ";base64," + base64.StdEncoding.EncodeToString(raw)
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]string{"url": dataURI}},
				},
			},
		},
		"max_tokens": 1200,
	}
	rawPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(rawPayload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := openAIHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("vision api error: %s", string(body))
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty vision response")
	}
	return result.Choices[0].Message.Content, nil
}

func analyzeDocumentWithOpenAI(ctx context.Context, apiKey, model, prompt, path, mime string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > 15<<20 {
		return "", fmt.Errorf("document too large for inline file request")
	}
	contentType := strings.TrimSpace(mime)
	if contentType == "" {
		contentType = mimeFromExt(path)
	}
	fileData := "data:" + firstText(contentType, "application/octet-stream") + ";base64," + base64.StdEncoding.EncodeToString(raw)
	payload := map[string]any{
		"model": firstText(model, "gpt-4.1-mini"),
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": prompt},
					{"type": "input_file", "filename": filepath.Base(path), "file_data": fileData},
				},
			},
		},
		"max_output_tokens": 1500,
	}
	rawPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(rawPayload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := openAIHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("document api error: %s", string(body))
	}
	text := extractOpenAIResponseText(body)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty document response")
	}
	return text, nil
}

func extractOpenAIResponseText(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if text := strings.TrimSpace(fmt.Sprintf("%v", payload["output_text"])); text != "" && text != "<nil>" {
		return text
	}
	output, _ := payload["output"].([]any)
	parts := make([]string, 0, 8)
	for _, item := range output {
		msg, _ := item.(map[string]any)
		content, _ := msg["content"].([]any)
		for _, rawContent := range content {
			block, _ := rawContent.(map[string]any)
			text := strings.TrimSpace(fmt.Sprintf("%v", block["text"]))
			if text == "" || text == "<nil>" {
				if nested, ok := block["text"].(map[string]any); ok {
					text = strings.TrimSpace(fmt.Sprintf("%v", nested["value"]))
				}
			}
			if text != "" && text != "<nil>" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func mimeFromExt(path string) string {
	switch normalizedDocumentExt(path) {
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".zip":
		return "application/zip"
	case ".tar.gz", ".tgz":
		return "application/gzip"
	case ".csv":
		return "text/csv"
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

func fetchNotionChunks(ctx context.Context, token, blockID string) ([]string, error) {
	blockID = strings.TrimSpace(blockID)
	if blockID == "" {
		return nil, fmt.Errorf("notion block/page id required")
	}
	url := "https://api.notion.com/v1/blocks/" + blockID + "/children?page_size=100"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Notion-Version", "2022-06-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("notion api error: %s", string(body))
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	results, _ := payload["results"].([]any)
	chunks := make([]string, 0, len(results))
	for _, item := range results {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(m)
		text := stripJSONRichText(string(raw))
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, text)
		}
	}
	return chunks, nil
}

func splitByCommaOrLine(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func stripHTML(raw string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	clean := re.ReplaceAllString(raw, " ")
	return collapseSpaces(clean)
}

func stripXMLTags(raw string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	return collapseSpaces(re.ReplaceAllString(raw, " "))
}

func stripJSONRichText(raw string) string {
	re := regexp.MustCompile(`\"plain_text\"\s*:\s*\"([^\"]+)\"`)
	matches := re.FindAllStringSubmatch(raw, -1)
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 {
			parts = append(parts, m[1])
		}
	}
	return collapseSpaces(strings.Join(parts, "\n"))
}

func collapseSpaces(raw string) string {
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := splitNonEmptyLines(raw)
	return strings.Join(lines, "\n")
}

func chunkText(raw string, size int) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if size <= 0 {
		size = 800
	}
	runes := []rune(raw)
	out := make([]string, 0, len(runes)/size+1)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		part := strings.TrimSpace(string(runes[i:end]))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitNonEmptyLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func extractNumbers(raw string) []float64 {
	re := regexp.MustCompile(`[-+]?[0-9]*\.?[0-9]+`)
	matches := re.FindAllString(raw, -1)
	nums := make([]float64, 0, len(matches))
	for _, m := range matches {
		v, err := strconv.ParseFloat(m, 64)
		if err != nil {
			continue
		}
		nums = append(nums, v)
	}
	sort.Float64s(nums)
	if len(nums) > 12 {
		nums = nums[:12]
	}
	return nums
}

func sanitizeMermaid(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\"", "")
	raw = strings.ReplaceAll(raw, "'", "")
	if raw == "" {
		return "Step"
	}
	return raw
}

func trimString(raw string, max int) string {
	raw = strings.TrimSpace(raw)
	if max <= 0 || len(raw) <= max {
		return raw
	}
	return raw[:max] + " ..."
}

func firstText(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func escapeHTML(raw string) string {
	repl := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return repl.Replace(raw)
}
