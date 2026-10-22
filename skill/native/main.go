package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL      = "https://foxapi.cn/v1"
	defaultModel        = "auto"
	defaultSize         = "1024x1024"
	defaultQuality      = "high"
	defaultOutputFormat = "png"
	outputSubdir        = "outputs/foxapi-imagegen"
	userAgent           = "codex-foxapi-imagegen-go/1.0"
)

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

type config struct {
	prompt            string
	apiMode           string
	imagesEndpoint    string
	requestParamsJSON string
	requestParams     []string
	images            []string
	imageURLs         []string
	cwd               string
	outputDir         string
	model             string
	size              string
	quality           string
	background        string
	apiKey            string
	endpoint          string
	baseURL           string
	outputFormat      string
	n                 int
	timeout           time.Duration
	mockB64           string
	toolParamsJSON    string
	toolParams        []string
}

type ImageError struct {
	kind    string
	message string
}

func (e *ImageError) Error() string { return e.message }

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println("Usage: foxapi-imagegen generate --prompt \"...\" [--model auto|MODEL_ID] [--tool-params-json '{...}']")
			return
		}
		printJSON(map[string]any{"ok": false, "error_type": "invalid_args", "message": err.Error()})
		os.Exit(2)
	}
	result, err := run(cfg)
	if err != nil {
		var imageErr *ImageError
		if errors.As(err, &imageErr) {
			printJSON(map[string]any{"ok": false, "error_type": imageErr.kind, "message": imageErr.message})
		} else {
			printJSON(map[string]any{"ok": false, "error_type": "unexpected_error", "message": err.Error()})
		}
		os.Exit(1)
	}
	printJSON(result)
}

func parseArgs(args []string) (config, error) {
	var cfg config
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		return cfg, flag.ErrHelp
	}
	if args[0] != "generate" {
		return cfg, fmt.Errorf("expected command 'generate'")
	}
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.prompt, "prompt", "", "Image prompt.")
	fs.StringVar(&cfg.apiMode, "api-mode", getenvDefault("FOXAPI_IMAGE_API_MODE", "responses"), "responses or images.")
	fs.StringVar(&cfg.imagesEndpoint, "images-endpoint", os.Getenv("FOXAPI_IMAGES_URL"), "OpenAI Images API endpoint.")
	fs.StringVar(&cfg.requestParamsJSON, "request-params-json", "", "Extra Images API request fields as a JSON object.")
	fs.Var((*stringList)(&cfg.requestParams), "request-param", "Extra Images API field as key=value; repeatable.")
	fs.Var((*stringList)(&cfg.images), "image", "Reference image path; repeatable.")
	fs.Var((*stringList)(&cfg.imageURLs), "image-url", "Reference image URL or data URL; repeatable.")
	fs.StringVar(&cfg.cwd, "cwd", "", "Current workspace folder.")
	fs.StringVar(&cfg.outputDir, "output-dir", "", "Output folder.")
	fs.StringVar(&cfg.model, "model", defaultModel, "Model ID, or auto.")
	fs.StringVar(&cfg.size, "size", getenvDefault("FOXAPI_IMAGE_SIZE", defaultSize), "Requested size.")
	fs.StringVar(&cfg.quality, "quality", getenvDefault("FOXAPI_IMAGE_QUALITY", defaultQuality), "Requested quality.")
	fs.StringVar(&cfg.background, "background", "", "Optional background setting.")
	fs.StringVar(&cfg.apiKey, "api-key", "", "API key override.")
	fs.StringVar(&cfg.endpoint, "endpoint", "", "Full Responses endpoint.")
	fs.StringVar(&cfg.baseURL, "base-url", "", "Base URL.")
	fs.StringVar(&cfg.outputFormat, "output-format", getenvDefault("FOXAPI_IMAGE_OUTPUT_FORMAT", defaultOutputFormat), "png, jpeg, or webp.")
	fs.IntVar(&cfg.n, "n", 1, "Number of images, generated as repeated requests.")
	timeoutSeconds := fs.Int("timeout", 240, "Request timeout in seconds.")
	fs.StringVar(&cfg.mockB64, "mock-b64", "", "Offline hook that saves this Base64 image.")
	fs.StringVar(&cfg.toolParamsJSON, "tool-params-json", "", "Extra image_generation tool parameters as a JSON object.")
	fs.Var((*stringList)(&cfg.toolParams), "tool-param", "Extra tool parameter as key=value; repeatable.")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, flag.ErrHelp
		}
		return cfg, err
	}
	modelWasSet := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "model" {
			modelWasSet = true
		}
	})
	if !modelWasSet {
		if cfg.apiMode == "images" {
			cfg.model = firstEnv([]string{"FOXAPI_IMAGE_API_MODEL", "FOXAPI_IMAGE_MODEL", "FOXAPI_MODEL"}, defaultModel)
		} else {
			cfg.model = firstEnv([]string{"FOXAPI_RESPONSES_MODEL", "FOXAPI_IMAGE_MODEL", "FOXAPI_MODEL"}, defaultModel)
		}
	}
	if strings.TrimSpace(cfg.prompt) == "" {
		return cfg, fmt.Errorf("--prompt is required")
	}
	if *timeoutSeconds < 1 {
		return cfg, fmt.Errorf("--timeout must be positive")
	}
	cfg.timeout = time.Duration(*timeoutSeconds) * time.Second
	return cfg, nil
}

func run(cfg config) (map[string]any, error) {
	if cfg.apiMode == "" {
		cfg.apiMode = "responses"
	}
	if cfg.apiMode != "responses" && cfg.apiMode != "images" {
		return nil, &ImageError{kind: "invalid_api_mode", message: "--api-mode must be responses or images."}
	}
	if cfg.n < 1 || cfg.n > 4 {
		return nil, &ImageError{kind: "invalid_n", message: "--n must be between 1 and 4."}
	}
	if !regexp.MustCompile("^\\d+x\\d+$").MatchString(cfg.size) {
		return nil, &ImageError{kind: "invalid_size", message: "Use WIDTHxHEIGHT for --size, for example 1024x1024."}
	}
	format := normalizeFormat(cfg.outputFormat)
	if format != "png" && format != "jpeg" && format != "webp" {
		return nil, &ImageError{kind: "invalid_output_format", message: "--output-format must be png, jpg, jpeg, or webp."}
	}
	for _, path := range cfg.images {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return nil, &ImageError{kind: "unreadable_reference_image", message: "Reference image is not readable: " + path}
		}
	}

	outputDir, err := resolveOutputDir(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}

	apiKey, credentialSource := "", "mock_b64"
	if cfg.mockB64 == "" {
		apiKey, credentialSource, err = resolveAPIKey(cfg.apiKey)
		if err != nil {
			return nil, err
		}
		cfg.model, err = resolveModel(cfg, apiKey)
		if err != nil {
			return nil, err
		}
	}
	extraParams, err := parseToolParams(cfg.toolParamsJSON, cfg.toolParams)
	if err != nil {
		return nil, &ImageError{kind: "invalid_tool_params", message: err.Error()}
	}
	requestParams, err := parseRequestParams(cfg.requestParamsJSON, cfg.requestParams)
	if err != nil {
		return nil, &ImageError{kind: "invalid_request_params", message: err.Error()}
	}
	if cfg.apiMode == "responses" && (strings.TrimSpace(cfg.requestParamsJSON) != "" || len(cfg.requestParams) > 0) {
		return nil, &ImageError{kind: "invalid_request_params", message: "Use --tool-params-json or --tool-param with the Responses API."}
	}
	if cfg.apiMode == "images" && (strings.TrimSpace(cfg.toolParamsJSON) != "" || len(cfg.toolParams) > 0) {
		return nil, &ImageError{kind: "invalid_tool_params", message: "Use --request-params-json or --request-param with the Images API."}
	}

	client := &http.Client{Timeout: cfg.timeout}
	saved := make([]map[string]any, 0, cfg.n)
	for index := 0; index < cfg.n; index++ {
		var item map[string]any
		if cfg.mockB64 != "" {
			item = map[string]any{"result": cfg.mockB64, "output_format": format, "status": "completed", "action": "generate"}
		} else {
			item, err = requestImage(cfg, apiKey, client, extraParams, requestParams)
			if err != nil {
				return nil, err
			}
		}
		saveFormat := format
		customFormat, hasCustomFormat := extraParams["output_format"].(string)
		if cfg.apiMode == "images" {
			customFormat, hasCustomFormat = requestParams["output_format"].(string)
		}
		if hasCustomFormat {
			saveFormat = normalizeFormat(customFormat)
		}
		savedItem, err := saveImage(item, cfg, outputDir, index, saveFormat)
		if err != nil {
			return nil, err
		}
		saved = append(saved, savedItem)
	}

	paths := make([]string, 0, len(saved))
	markdown := make([]string, 0, len(saved))
	savedMarkdown := make([]string, 0, len(saved))
	for _, item := range saved {
		path := item["path"].(string)
		paths = append(paths, path)
		markdown = append(markdown, "![Generated image]("+path+")")
		savedMarkdown = append(savedMarkdown, "Saved to: ["+path+"]("+path+")")
	}
	folder := slashPath(outputDir)
	return map[string]any{
		"ok":                    true,
		"mode":                  "generate",
		"provider":              "foxapi",
		"api_mode":              cfg.apiMode,
		"endpoint":              safeEndpoint(selectedEndpoint(cfg)),
		"model":                 cfg.model,
		"credential_source":     credentialSource,
		"requested_size":        cfg.size,
		"requested_quality":     cfg.quality,
		"reference_image_count": len(cfg.images) + len(cfg.imageURLs),
		"images":                saved,
		"paths":                 paths,
		"markdown":              markdown,
		"saved_markdown":        savedMarkdown,
		"folder_markdown":       "[Open folder](" + folder + ")",
	}, nil
}

func resolveOutputDir(cfg config) (string, error) {
	dir := cfg.outputDir
	if dir == "" {
		dir = os.Getenv("FOXAPI_IMAGE_OUTPUT_DIR")
	}
	if dir == "" {
		cwd := cfg.cwd
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		dir = filepath.Join(cwd, filepath.FromSlash(outputSubdir))
	}
	return filepath.Abs(dir)
}

func resolveAPIKey(override string) (string, string, error) {
	candidates := []struct{ source, value string }{
		{"--api-key", override},
		{"FOXAPI_API_KEY", os.Getenv("FOXAPI_API_KEY")},
		{"FOXAPI_KEY", os.Getenv("FOXAPI_KEY")},
		{"foxapi_config", readSavedAPIKey()},
		{"OPENAI_API_KEY", os.Getenv("OPENAI_API_KEY")},
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.value) != "" {
			return strings.TrimSpace(candidate.value), candidate.source, nil
		}
	}
	if key := readCodexAuthKey(); key != "" {
		return key, "codex_auth_json", nil
	}
	return "", "", &ImageError{kind: "missing_api_key", message: "No API key found. Run the Skill setup command, or set FOXAPI_API_KEY / OPENAI_API_KEY."}
}

func readSavedAPIKey() string {
	home, _ := os.UserHomeDir()
	paths := []string{}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		paths = append(paths, filepath.Join(codexHome, "foxapi-imagegen", "config.json"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".codex", "foxapi-imagegen", "config.json"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var object map[string]any
		if json.Unmarshal(data, &object) != nil {
			continue
		}
		for _, field := range []string{"apiKey", "FOXAPI_API_KEY", "FOXAPI_KEY"} {
			if value, ok := object[field].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func readCodexAuthKey() string {
	home, _ := os.UserHomeDir()
	paths := []string{}
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		paths = append(paths, filepath.Join(codexHome, "auth.json"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".codex", "auth.json"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var value any
		if json.Unmarshal(data, &value) != nil {
			continue
		}
		if key := findAPIKey(value); key != "" {
			return key
		}
	}
	return ""
}

func findAPIKey(value any) string {
	names := map[string]bool{"OPENAI_API_KEY": true, "FOXAPI_API_KEY": true, "FOXAPI_KEY": true, "api_key": true, "openai_api_key": true, "foxapi_api_key": true}
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if names[key] {
				if text, ok := child.(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
		}
		for _, child := range item {
			if found := findAPIKey(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range item {
			if found := findAPIKey(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func resolveBaseURL(cfg config) string {
	base := cfg.baseURL
	if base == "" {
		base = os.Getenv("FOXAPI_BASE_URL")
	}
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return defaultBaseURL
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/responses", "/images/generations", "/images/edits"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
		}
	}
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}

func resolveImagesURL(cfg config, edit bool) string {
	endpoint := cfg.imagesEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("FOXAPI_IMAGES_URL")
	}
	route := "/images/generations"
	if edit {
		route = "/images/edits"
	}
	if endpoint == "" {
		return resolveBaseURL(cfg) + route
	}
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return strings.TrimRight(endpoint, "/") + route
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/images/generations", "/images/edits"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
		}
	}
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = path + route
	} else if strings.HasSuffix(path, "/images") {
		parsed.Path = path + strings.TrimPrefix(route, "/images")
	} else {
		parsed.Path = path + "/v1" + route
	}
	parsed.RawPath = ""
	return parsed.String()
}

func selectedEndpoint(cfg config) string {
	if cfg.apiMode == "images" {
		return resolveImagesURL(cfg, len(cfg.images) > 0)
	}
	return resolveResponsesURL(cfg)
}

func resolveResponsesURL(cfg config) string {
	endpoint := cfg.endpoint
	if endpoint == "" {
		endpoint = os.Getenv("FOXAPI_RESPONSES_URL")
	}
	if endpoint != "" {
		endpoint = strings.TrimRight(endpoint, "/")
		if strings.HasSuffix(endpoint, "/responses") {
			return endpoint
		}
		if strings.HasSuffix(endpoint, "/v1") {
			return endpoint + "/responses"
		}
		return endpoint + "/v1/responses"
	}
	base := cfg.baseURL
	if base == "" {
		base = os.Getenv("FOXAPI_BASE_URL")
	}
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

func resolveModelsURL(cfg config) (string, error) {
	var raw string
	if cfg.apiMode == "images" {
		raw = cfg.imagesEndpoint
		if raw == "" {
			raw = os.Getenv("FOXAPI_IMAGES_URL")
		}
		if raw == "" {
			return resolveBaseURL(cfg) + "/models", nil
		}
		parsed, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil {
			return "", &ImageError{kind: "model_discovery_error", message: "The Images endpoint URL is invalid."}
		}
		path := strings.TrimRight(parsed.Path, "/")
		for _, suffix := range []string{"/images/generations", "/images/edits"} {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimSuffix(path, suffix)
			}
		}
		if !strings.HasSuffix(path, "/v1") {
			path += "/v1"
		}
		parsed.Path = path + "/models"
		parsed.RawPath = ""
		return parsed.String(), nil
	}
	responsesURL, err := url.Parse(resolveResponsesURL(cfg))
	if err != nil {
		return "", &ImageError{kind: "model_discovery_error", message: "The Responses endpoint URL is invalid."}
	}
	path := strings.TrimRight(responsesURL.Path, "/")
	if !strings.HasSuffix(path, "/responses") {
		return "", &ImageError{kind: "model_discovery_error", message: "The Responses endpoint must end with /responses to discover models."}
	}
	responsesURL.Path = strings.TrimSuffix(path, "/responses") + "/models"
	responsesURL.RawPath = ""
	return responsesURL.String(), nil
}

func safeEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

func resolveModel(cfg config, apiKey string) (string, error) {
	requested := strings.TrimSpace(cfg.model)
	if !strings.EqualFold(requested, "auto") && !strings.EqualFold(requested, "latest") {
		return requested, nil
	}
	modelsURL, err := resolveModelsURL(cfg)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return "", &ImageError{kind: "model_discovery_error", message: "Could not construct the FoxAPI model catalog request."}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", &ImageError{kind: "model_discovery_error", message: "Could not reach the FoxAPI model catalog: " + err.Error() + ". Set --model to a specific model ID to continue."}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ImageError{kind: "model_discovery_error", message: fmt.Sprintf("Could not read the FoxAPI model catalog (HTTP %d). Set --model to a specific model ID to continue.", resp.StatusCode)}
	}
	var catalog any
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return "", &ImageError{kind: "model_discovery_error", message: "FoxAPI returned an unreadable model catalog."}
	}
	entries := modelEntries(catalog)
	candidates := []map[string]any{}
	for _, entry := range entries {
		id := asString(entry["id"])
		if cfg.apiMode == "images" && isImageAPICatalogModel(id) || cfg.apiMode != "images" && isGeneralGPTModel(id) {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		if cfg.apiMode == "images" {
			return "", &ImageError{kind: "model_discovery_error", message: "FoxAPI did not list a recognized image model (gpt-image-* or dall-e-*). Set --model to the exact model ID."}
		}
		return "", &ImageError{kind: "model_discovery_error", message: "FoxAPI did not list a general GPT model. Set --model to a specific model ID."}
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if compareModels(candidate, best) > 0 {
			best = candidate
		}
	}
	return asString(best["id"]), nil
}

func isImageAPICatalogModel(id string) bool {
	id = strings.ToLower(id)
	return strings.HasPrefix(id, "gpt-image-") || strings.HasPrefix(id, "dall-e-") || strings.HasPrefix(id, "dalle-")
}

func modelEntries(catalog any) []map[string]any {
	var raw any = catalog
	if object, ok := catalog.(map[string]any); ok {
		raw = object["data"]
		if raw == nil {
			raw = object["models"]
		}
	}
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	entries := []map[string]any{}
	for _, value := range values {
		if entry, ok := value.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

var generalGPTID = regexp.MustCompile("(?i)^gpt-\\d+(?:\\.\\d+)?(?:-[a-z0-9]+(?:-[a-z0-9]+)*)?$")

func isGeneralGPTModel(id string) bool {
	if !generalGPTID.MatchString(id) {
		return false
	}
	excluded := map[string]bool{"image": true, "audio": true, "tts": true, "transcribe": true, "transcription": true, "codex": true, "embedding": true, "realtime": true, "whisper": true}
	for _, part := range strings.Split(strings.ToLower(id), "-") {
		if excluded[part] {
			return false
		}
	}
	return true
}

func compareModels(a, b map[string]any) int {
	aCreated, bCreated := integer(a["created"]), integer(b["created"])
	if aCreated != bCreated {
		if aCreated > bCreated {
			return 1
		}
		return -1
	}
	aVersion, bVersion := modelVersion(asString(a["id"])), modelVersion(asString(b["id"]))
	if comparison := compareIntSlices(aVersion, bVersion); comparison != 0 {
		return comparison
	}
	priority := map[string]int{"astra": 4, "sol": 3, "terra": 2, "luna": 1}
	aVariant, bVariant := modelVariant(asString(a["id"]), priority), modelVariant(asString(b["id"]), priority)
	if aVariant != bVariant {
		if aVariant > bVariant {
			return 1
		}
		return -1
	}
	return strings.Compare(asString(a["id"]), asString(b["id"]))
}

func integer(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func modelVersion(id string) []int {
	match := regexp.MustCompile("(?i)^gpt-(?:image-)?(\\d+(?:\\.\\d+)?)").FindStringSubmatch(id)
	if len(match) < 2 {
		return nil
	}
	parts := strings.Split(match[1], ".")
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		n, _ := strconv.Atoi(part)
		version = append(version, n)
	}
	return version
}

func compareIntSlices(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return 0
}

func modelVariant(id string, priority map[string]int) int {
	for _, part := range strings.Split(strings.ToLower(id), "-") {
		if priority[part] > 0 {
			return priority[part]
		}
	}
	return 0
}

func parseToolParams(rawJSON string, pairs []string) (map[string]any, error) {
	result := map[string]any{}
	if strings.TrimSpace(rawJSON) != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(rawJSON), &extra); err != nil || extra == nil {
			return nil, fmt.Errorf("--tool-params-json must be a JSON object")
		}
		for key, value := range extra {
			if err := setToolParam(result, key, value); err != nil {
				return nil, err
			}
		}
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--tool-param must use key=value")
		}
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) != nil {
			decoded = value
		}
		if err := setToolParam(result, strings.TrimSpace(key), decoded); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func parseRequestParams(rawJSON string, pairs []string) (map[string]any, error) {
	result := map[string]any{}
	if strings.TrimSpace(rawJSON) != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(rawJSON), &extra); err != nil || extra == nil {
			return nil, fmt.Errorf("--request-params-json must be a JSON object")
		}
		for key, value := range extra {
			if err := setRequestParam(result, key, value); err != nil {
				return nil, err
			}
		}
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--request-param must use key=value")
		}
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) != nil {
			decoded = value
		}
		if err := setRequestParam(result, strings.TrimSpace(key), decoded); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func setRequestParam(target map[string]any, key string, value any) error {
	reserved := map[string]bool{"model": true, "prompt": true, "n": true, "image": true}
	if reserved[strings.ToLower(key)] {
		return fmt.Errorf("request parameters cannot replace the reserved %q field", key)
	}
	target[key] = value
	return nil
}

func setToolParam(target map[string]any, key string, value any) error {
	if key == "type" || key == "action" {
		return fmt.Errorf("tool parameters cannot replace the reserved %q field", key)
	}
	target[key] = value
	return nil
}

func requestImage(cfg config, apiKey string, client *http.Client, extraParams, requestParams map[string]any) (map[string]any, error) {
	if cfg.apiMode == "images" {
		return requestImagesAPI(cfg, apiKey, client, requestParams)
	}
	input, err := buildInput(cfg)
	if err != nil {
		return nil, err
	}
	tool := map[string]any{"type": "image_generation", "action": "generate", "size": cfg.size, "quality": cfg.quality}
	if cfg.background != "" {
		tool["background"] = cfg.background
	}
	if cfg.outputFormat != "" {
		tool["output_format"] = normalizeFormat(cfg.outputFormat)
	}
	for key, value := range extraParams {
		tool[key] = value
	}
	payload := map[string]any{
		"model":       cfg.model,
		"input":       input,
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "image_generation"},
		"stream":      true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, resolveResponsesURL(cfg), bytes.NewReader(body))
	if err != nil {
		return nil, &ImageError{kind: "request_error", message: "Could not construct the FoxAPI Responses request."}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, &ImageError{kind: "network_error", message: "Could not reach FoxAPI: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return nil, &ImageError{kind: "http_error", message: fmt.Sprintf("FoxAPI returned HTTP %d: %s", resp.StatusCode, detail)}
	}
	return readImageSSE(resp.Body)
}

func requestImagesAPI(cfg config, apiKey string, client *http.Client, extraParams map[string]any) (map[string]any, error) {
	if len(cfg.imageURLs) > 0 {
		return nil, &ImageError{kind: "unsupported_reference", message: "The Images API edit route requires a local --image file; use --api-mode responses for image URLs."}
	}
	if len(cfg.images) > 1 {
		return nil, &ImageError{kind: "unsupported_reference", message: "The Images API edit route accepts one local reference image; use --api-mode responses for multiple references."}
	}
	edit := len(cfg.images) == 1
	endpoint := resolveImagesURL(cfg, edit)
	fields := map[string]any{
		"model":  cfg.model,
		"prompt": cfg.prompt,
		"n":      1,
		"size":   cfg.size,
	}
	if cfg.quality != "" {
		fields["quality"] = cfg.quality
	}
	if cfg.outputFormat != "" {
		fields["output_format"] = normalizeFormat(cfg.outputFormat)
	}
	if cfg.background != "" {
		fields["background"] = cfg.background
	}
	for key, value := range extraParams {
		fields[key] = value
	}

	var body io.Reader
	contentType := "application/json"
	if edit {
		buffer := &bytes.Buffer{}
		writer := multipart.NewWriter(buffer)
		for key, value := range fields {
			if err := writer.WriteField(key, formValue(value)); err != nil {
				return nil, &ImageError{kind: "request_error", message: "Could not encode Images API request fields."}
			}
		}
		file, err := os.Open(cfg.images[0])
		if err != nil {
			return nil, &ImageError{kind: "unreadable_reference_image", message: "Could not read reference image: " + cfg.images[0]}
		}
		mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(cfg.images[0])))
		if parsed, _, parseErr := mime.ParseMediaType(mediaType); parseErr == nil {
			mediaType = parsed
		}
		if !strings.HasPrefix(mediaType, "image/") {
			buffer := make([]byte, 512)
			n, readErr := file.Read(buffer)
			if readErr != nil && readErr != io.EOF {
				file.Close()
				return nil, &ImageError{kind: "unreadable_reference_image", message: "Could not inspect reference image: " + cfg.images[0]}
			}
			mediaType = http.DetectContentType(buffer[:n])
			if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
				file.Close()
				return nil, &ImageError{kind: "unreadable_reference_image", message: "Could not rewind reference image: " + cfg.images[0]}
			}
		}
		if !strings.HasPrefix(mediaType, "image/") {
			file.Close()
			return nil, &ImageError{kind: "unsupported_reference", message: "Reference file is not a recognized image: " + cfg.images[0]}
		}
		partHeader := make(textproto.MIMEHeader)
		partHeader.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "image", "filename": filepath.Base(cfg.images[0])}))
		partHeader.Set("Content-Type", mediaType)
		part, err := writer.CreatePart(partHeader)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		file.Close()
		if err != nil {
			return nil, &ImageError{kind: "request_error", message: "Could not encode the reference image for the Images API."}
		}
		if err := writer.Close(); err != nil {
			return nil, &ImageError{kind: "request_error", message: "Could not finish the Images API edit request."}
		}
		body = buffer
		contentType = writer.FormDataContentType()
	} else {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, &ImageError{kind: "request_error", message: "Could not encode the Images API request."}
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return nil, &ImageError{kind: "request_error", message: "Could not construct the Images API request."}
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, &ImageError{kind: "network_error", message: "Could not reach FoxAPI Images API."}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2000))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return nil, &ImageError{kind: "http_error", message: fmt.Sprintf("FoxAPI Images API returned HTTP %d: %s", response.StatusCode, detail)}
	}
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(response.Body, 128*1024*1024))
	if err := decoder.Decode(&payload); err != nil {
		return nil, &ImageError{kind: "response_error", message: "Could not decode the FoxAPI Images API response: " + err.Error()}
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) == 0 {
		if apiErr, exists := payload["error"]; exists {
			return nil, &ImageError{kind: "api_error", message: formatAPIError(apiErr)}
		}
		return nil, &ImageError{kind: "missing_image_result", message: "FoxAPI Images API response did not include data[0].b64_json or data[0].url."}
	}
	image, ok := data[0].(map[string]any)
	if !ok {
		return nil, &ImageError{kind: "response_error", message: "FoxAPI returned an invalid Images API image item."}
	}
	result := asString(image["b64_json"])
	if result == "" {
		imageURL := asString(image["url"])
		if imageURL == "" {
			return nil, &ImageError{kind: "missing_image_result", message: "FoxAPI Images API response did not include image data."}
		}
		result, err = downloadImageURL(client, imageURL)
		if err != nil {
			return nil, err
		}
	}
	item := map[string]any{"result": result, "output_format": fields["output_format"], "action": "generate"}
	for _, key := range []string{"id", "status", "size", "quality", "revised_prompt"} {
		if value, exists := image[key]; exists {
			item[key] = value
		}
	}
	return item, nil
}

func formValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func downloadImageURL(client *http.Client, imageURL string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, imageURL, nil)
	if err != nil {
		return "", &ImageError{kind: "download_error", message: "FoxAPI returned an invalid image URL."}
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", &ImageError{kind: "download_error", message: "Could not download the image URL returned by FoxAPI."}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &ImageError{kind: "download_error", message: fmt.Sprintf("The image URL returned by FoxAPI responded with HTTP %d.", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 128*1024*1024))
	if err != nil {
		return "", &ImageError{kind: "download_error", message: "Could not read the image URL returned by FoxAPI."}
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func buildInput(cfg config) (any, error) {
	if len(cfg.images) == 0 && len(cfg.imageURLs) == 0 {
		return cfg.prompt, nil
	}
	content := []map[string]any{{"type": "input_text", "text": cfg.prompt}}
	for _, path := range cfg.images {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &ImageError{kind: "unreadable_reference_image", message: "Could not read reference image: " + path}
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": imageDataURL(path, data)})
	}
	for _, imageURL := range cfg.imageURLs {
		content = append(content, map[string]any{"type": "input_image", "image_url": imageURL})
	}
	return []map[string]any{{"role": "user", "content": content}}, nil
}

func imageDataURL(path string, data []byte) string {
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	}
	if !strings.HasPrefix(mediaType, "image/") {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func readImageSSE(reader io.Reader) (map[string]any, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 128*1024*1024)
	dataLines := []string{}
	var lastItem map[string]any
	var lastError any
	process := func() {
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = []string{}
		if data == "" || data == "[DONE]" {
			return
		}
		var decoded any
		if json.Unmarshal([]byte(data), &decoded) != nil {
			return
		}
		if object, ok := decoded.(map[string]any); ok {
			if value, exists := object["error"]; exists {
				lastError = value
			}
		}
		items := findImageItems(decoded)
		if len(items) > 0 {
			lastItem = items[len(items)-1]
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			process()
			if lastItem != nil {
				return lastItem, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) > 0 {
		process()
	}
	if lastItem != nil {
		return lastItem, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, &ImageError{kind: "stream_error", message: "Could not read FoxAPI's image response stream: " + err.Error()}
	}
	if lastError != nil {
		return nil, &ImageError{kind: "api_error", message: formatAPIError(lastError)}
	}
	return nil, &ImageError{kind: "missing_image_result", message: "FoxAPI response did not contain image_generation_call.result."}
}

func findImageItems(value any) []map[string]any {
	found := []map[string]any{}
	switch item := value.(type) {
	case map[string]any:
		if item["type"] == "image_generation_call" && item["result"] != nil {
			found = append(found, item)
		}
		for _, child := range item {
			found = append(found, findImageItems(child)...)
		}
	case []any:
		for _, child := range item {
			found = append(found, findImageItems(child)...)
		}
	}
	return found
}

func formatAPIError(value any) string {
	if object, ok := value.(map[string]any); ok {
		if message, ok := object["message"].(string); ok && message != "" {
			return message
		}
		if kind, ok := object["type"].(string); ok && kind != "" {
			return kind
		}
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func saveImage(item map[string]any, cfg config, outputDir string, index int, requestedFormat string) (map[string]any, error) {
	encoded, ok := item["result"].(string)
	if !ok || encoded == "" {
		return nil, &ImageError{kind: "missing_image_result", message: "FoxAPI response did not contain image_generation_call.result."}
	}
	if strings.HasPrefix(encoded, "data:") {
		if comma := strings.Index(encoded, ","); comma >= 0 {
			encoded = encoded[comma+1:]
		}
	}
	imageBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, &ImageError{kind: "invalid_image_result", message: "FoxAPI returned image data that could not be decoded."}
	}
	outputFormat := requestedFormat
	if actual, ok := item["output_format"].(string); ok && actual != "" {
		outputFormat = normalizeFormat(actual)
	}
	ext := outputFormat
	if ext == "jpeg" {
		ext = "jpg"
	}
	now := time.Now()
	stamp := now.Format("20060102-150405") + fmt.Sprintf("-%06d", now.Nanosecond()/1000%1000000)
	name := fmt.Sprintf("%s-%d-%s.%s", stamp, index+1, slugify(cfg.prompt), ext)
	path := filepath.Join(outputDir, name)
	if err := os.WriteFile(path, imageBytes, 0o644); err != nil {
		return nil, err
	}
	result := map[string]any{"path": slashPath(path), "bytes": len(imageBytes), "output_format": outputFormat}
	for _, key := range []string{"id", "status", "action", "size", "quality"} {
		if value, exists := item[key]; exists {
			outKey := key
			if key == "size" {
				outKey = "actual_size"
			}
			if key == "quality" {
				outKey = "actual_quality"
			}
			result[outKey] = value
		}
	}
	return result, nil
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			builder.WriteRune(r)
			lastDash = false
		} else if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "-")
}

func slashPath(path string) string { return filepath.ToSlash(path) }

func normalizeFormat(value string) string {
	value = strings.ToLower(value)
	if value == "jpg" {
		return "jpeg"
	}
	return value
}

func getenvDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func firstEnv(names []string, fallback string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return fallback
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func printJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
