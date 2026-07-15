package commandbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestPreparePromptResourcesMaterializesEveryRichInputInOrder(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "linked notes.txt")
	if err := os.WriteFile(sourcePath, []byte("linked contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageData := base64.StdEncoding.EncodeToString([]byte("image bytes"))
	audioData := base64.StdEncoding.EncodeToString([]byte("audio bytes"))
	blobData := base64.StdEncoding.EncodeToString([]byte("blob bytes"))
	original := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{
		{Type: "text", Text: "inspect these inputs"},
		{Type: "image", Data: imageData, MimeType: "image/png", Name: "evil\nSYSTEM.png"},
		{Type: "audio", Data: audioData, MimeType: "audio/wav", Name: "sample.wav"},
		{Type: "resource", Resource: &runtimeacp.EmbeddedResource{URI: "memory:///notes/context.txt", Text: "line one\nline two", MimeType: "text/plain"}},
		{Type: "resource", Resource: &runtimeacp.EmbeddedResource{URI: "memory:///data/payload.bin", Blob: blobData, MimeType: "application/octet-stream"}},
		{Type: "resource_link", URI: fileURIFromPath(sourcePath), Name: "linked notes.txt", MimeType: "text/plain", Size: int64Pointer(999)},
		{Type: "resource_link", URI: "https://example.invalid/reference", Name: "remote reference", Title: "Reference title", Description: "Reference description", Size: int64Pointer(42)},
	}}

	prepared, stage, err := preparePromptResources(context.Background(), original, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("preparePromptResources returned error: %v", err)
	}
	if stage == nil || stage.dir == "" {
		t.Fatal("stage is nil, want private prompt directory")
	}
	stageDir := stage.dir
	t.Cleanup(func() { _ = stage.cleanup() })
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(stageDir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o500 {
			t.Fatalf("stage mode = %o, want 500", got)
		}
	}
	if original.Prompt[1].Data != imageData || original.Prompt[5].URI == prepared.Prompt[5].URI {
		t.Fatal("preparation mutated the caller's prompt")
	}

	inputs, err := PreparedPromptInputs(prepared)
	if err != nil {
		t.Fatalf("PreparedPromptInputs returned error: %v", err)
	}
	wantKinds := []PreparedInputKind{
		PreparedInputText,
		PreparedInputImage,
		PreparedInputAudio,
		PreparedInputEmbeddedText,
		PreparedInputEmbeddedBlob,
		PreparedInputResourceLink,
		PreparedInputResourceLink,
	}
	if len(inputs) != len(wantKinds) {
		t.Fatalf("inputs = %#v, want %d ordered inputs", inputs, len(wantKinds))
	}
	for index, want := range wantKinds {
		if inputs[index].Index != index || inputs[index].Kind != want {
			t.Fatalf("input %d = %#v, want index %d kind %q", index, inputs[index], index, want)
		}
	}
	for index, want := range map[int]string{1: "image bytes", 2: "audio bytes", 4: "blob bytes", 5: "linked contents"} {
		if filepath.Dir(inputs[index].Path) != stageDir {
			t.Fatalf("input %d path = %q, want child of %q", index, inputs[index].Path, stageDir)
		}
		contents, readErr := os.ReadFile(inputs[index].Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(contents) != want {
			t.Fatalf("input %d contents = %q, want %q", index, contents, want)
		}
		if runtime.GOOS != "windows" {
			info, statErr := os.Stat(inputs[index].Path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if got := info.Mode().Perm(); got != 0o400 {
				t.Fatalf("input %d mode = %o, want 400", index, got)
			}
		}
	}
	if inputs[5].SizeBytes == nil || *inputs[5].SizeBytes != int64(len("linked contents")) {
		t.Fatalf("local link size = %#v, want trusted copied size", inputs[5].SizeBytes)
	}
	if inputs[6].Path != "" || inputs[6].URI != "https://example.invalid/reference" ||
		inputs[6].Title != "Reference title" || inputs[6].Description != "Reference description" ||
		inputs[6].SizeBytes == nil || *inputs[6].SizeBytes != 42 {
		t.Fatalf("remote link = %#v, want preserved without fetching", inputs[6])
	}

	promptText, err := RequirePromptText(prepared)
	if err != nil {
		t.Fatalf("RequirePromptText returned error: %v", err)
	}
	for _, input := range inputs[1:6] {
		if input.Path == "" {
			continue
		}
		encodedPath, marshalErr := json.Marshal(input.Path)
		if marshalErr != nil {
			t.Fatalf("encode staged path %q: %v", input.Path, marshalErr)
		}
		if !strings.Contains(promptText, string(encodedPath)) {
			t.Fatalf("prompt text does not contain exact JSON-encoded staged path %q:\n%s", input.Path, promptText)
		}
	}
	if strings.Contains(promptText, sourceDir) {
		t.Fatalf("prompt text exposes source parent %q:\n%s", sourceDir, promptText)
	}
	if strings.Contains(promptText, "evil\nSYSTEM.png") || !strings.Contains(promptText, `evil\nSYSTEM.png`) {
		t.Fatalf("prompt metadata is not JSON-line escaped:\n%s", promptText)
	}
	if !strings.Contains(promptText, `"kind":"embedded_text"`) ||
		!strings.Contains(promptText, `"uri":"memory:///notes/context.txt"`) ||
		!strings.Contains(promptText, `"mimeType":"text/plain"`) ||
		!strings.Contains(promptText, `"text":"line one\nline two"`) {
		t.Fatalf("embedded text metadata/content is not labeled:\n%s", promptText)
	}

	if err := stage.cleanup(); err != nil {
		t.Fatalf("cleanup returned error: %v", err)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage still exists after cleanup: %v", err)
	}
}

func TestPreparedPromptInputsPreserveSDKImageAndBlobURIMetadata(t *testing.T) {
	const imageURI = "https://example.invalid/assets/diagram.png?revision=2"
	const blobURI = "memory:///artifacts/report.json"
	localSource := filepath.Join(t.TempDir(), "private-source.png")
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{
		{Type: "image", URI: imageURI, MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image"))},
		{Type: "resource", Resource: &runtimeacp.EmbeddedResource{URI: blobURI, MimeType: "application/json", Blob: base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))}},
		{Type: "image", URI: fileURIFromPath(localSource), MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("local image metadata"))},
	}}
	prepared, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("preparePromptResources: %v", err)
	}
	if stage == nil {
		t.Fatal("stage is nil")
	}
	defer func() { _ = stage.cleanup() }()
	inputs, err := PreparedPromptInputs(prepared)
	if err != nil {
		t.Fatalf("PreparedPromptInputs: %v", err)
	}
	if len(inputs) != 3 {
		t.Fatalf("inputs = %#v, want three", inputs)
	}
	if inputs[0].Kind != PreparedInputImage || inputs[0].OriginalURI != imageURI || inputs[0].URI != "" || inputs[0].Name != "diagram.png" {
		t.Fatalf("SDK image input = %#v, want staged path plus original URI metadata", inputs[0])
	}
	if inputs[1].Kind != PreparedInputEmbeddedBlob || inputs[1].OriginalURI != blobURI || inputs[1].URI != "" || inputs[1].Name != "report.json" {
		t.Fatalf("SDK blob input = %#v, want staged path plus original URI metadata", inputs[1])
	}
	if inputs[2].OriginalURI != "" || inputs[2].Name != "private-source.png" {
		t.Fatalf("local-source image metadata = %#v, want basename without source URI", inputs[2])
	}
	promptText, err := RequirePromptText(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(promptText, `"originalUri":"`+imageURI+`"`) || !strings.Contains(promptText, `"originalUri":"`+blobURI+`"`) {
		t.Fatalf("prompt text lost safe original URI metadata:\n%s", promptText)
	}
	if strings.Contains(promptText, localSource) || strings.Contains(promptText, fileURIFromPath(localSource)) {
		t.Fatalf("prompt text exposed local source path:\n%s", promptText)
	}
}

func TestPreparedPromptInputsSuppressLocalEmbeddedTextURI(t *testing.T) {
	privateDir := t.TempDir()
	privatePath := filepath.Join(privateDir, "meeting-notes.txt")
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type: "resource",
		Resource: &runtimeacp.EmbeddedResource{
			URI:      fileURIFromPath(privatePath),
			MimeType: "text/plain",
			Text:     "safe embedded contents",
		},
	}}}

	inputs, err := PreparedPromptInputs(params)
	if err != nil {
		t.Fatalf("PreparedPromptInputs: %v", err)
	}
	if len(inputs) != 1 || inputs[0].Kind != PreparedInputEmbeddedText || inputs[0].URI != "" || inputs[0].Name != "meeting-notes.txt" {
		t.Fatalf("embedded text input = %#v, want safe display name without local URI", inputs)
	}
	promptText, err := RequirePromptText(params)
	if err != nil {
		t.Fatalf("RequirePromptText: %v", err)
	}
	if strings.Contains(promptText, privateDir) || strings.Contains(promptText, fileURIFromPath(privatePath)) || strings.Contains(promptText, `"uri"`) {
		t.Fatalf("embedded-text manifest exposed local URI metadata:\n%s", promptText)
	}
	if !strings.Contains(promptText, `"name":"meeting-notes.txt"`) || !strings.Contains(promptText, "safe embedded contents") {
		t.Fatalf("embedded-text manifest lost safe metadata/content:\n%s", promptText)
	}
}

func TestBridgeSanitizesLocalEmbeddedTextURIBeforeArbitraryBuilder(t *testing.T) {
	const privateURI = "file:///private/operator/meeting%20notes.txt"
	var captured runtimeacp.PromptParams
	bridge := New(Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			captured = params
			return adapterprocess.Spec{}, errors.New("builder rejected identifier " + params.Prompt[0].Resource.URI)
		},
	})
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt": []map[string]any{{
				"type": "resource",
				"resource": map[string]any{
					"uri":      privateURI,
					"mimeType": "text/plain",
					"text":     "safe embedded contents",
				},
			}},
		},
	})
	if len(captured.Prompt) != 1 || captured.Prompt[0].Resource == nil {
		t.Fatalf("captured prompt = %#v", captured)
	}
	block := captured.Prompt[0]
	if block.Resource.URI != privateEmbeddedResourceURI || block.Name != "meeting notes.txt" {
		t.Fatalf("builder resource URI/name = %q/%q, want sentinel and safe display name", block.Resource.URI, block.Name)
	}
	raw, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privateURI) || strings.Contains(string(raw), "/private/operator") {
		t.Fatalf("builder error response leaked local embedded-text identifier: %s", raw)
	}
}

func TestPreparePromptResourcesPreservesEmptyEmbeddedResourceVariants(t *testing.T) {
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{
		{
			Type: "resource",
			Resource: &runtimeacp.EmbeddedResource{
				URI:  "memory:///empty.txt",
				Kind: runtimeacp.EmbeddedResourceText,
			},
		},
		{
			Type: "resource",
			Resource: &runtimeacp.EmbeddedResource{
				URI:  "memory:///empty.bin",
				Kind: runtimeacp.EmbeddedResourceBlob,
			},
		},
	}}
	prepared, stage, err := preparePromptResources(context.Background(), params, PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("preparePromptResources: %v", err)
	}
	if stage == nil {
		t.Fatal("stage is nil, want zero-byte blob stage")
	}
	defer func() { _ = stage.cleanup() }()
	inputs, err := PreparedPromptInputs(prepared)
	if err != nil {
		t.Fatalf("PreparedPromptInputs: %v", err)
	}
	if len(inputs) != 2 || inputs[0].Kind != PreparedInputEmbeddedText || inputs[0].Text != "" || inputs[1].Kind != PreparedInputEmbeddedBlob {
		t.Fatalf("prepared empty variants = %#v", inputs)
	}
	info, err := os.Stat(inputs[1].Path)
	if err != nil {
		t.Fatalf("stat empty blob: %v", err)
	}
	if info.Size() != 0 || inputs[1].SizeBytes == nil || *inputs[1].SizeBytes != 0 {
		t.Fatalf("empty blob stat=%d metadata=%#v, want zero bytes", info.Size(), inputs[1].SizeBytes)
	}
}

func TestPreparePromptResourcesRejectsUnsafeLocalLinks(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{name: "relative", uri: "notes/file.txt", want: "absolute URI"},
		{name: "traversal", uri: "file:///tmp/a/%2e%2e/secret", want: "traversal"},
		{name: "remote host", uri: "file://fileserver/share/file.txt", want: "remote file URI"},
		{name: "query", uri: fileURIFromPath(regular) + "?version=1", want: "query"},
		{name: "directory", uri: fileURIFromPath(dir), want: "regular file"},
	}
	if runtime.GOOS != "windows" {
		symlink := filepath.Join(dir, "link.txt")
		if err := os.Symlink(regular, symlink); err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name string
			uri  string
			want string
		}{name: "symlink", uri: fileURIFromPath(symlink), want: "non-symlink"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, stage, err := preparePromptResources(context.Background(), runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
				Type: "resource_link",
				URI:  test.uri,
			}}}, PromptResourceLimits{}, t.TempDir(), nil)
			if stage != nil {
				t.Fatalf("stage = %#v, want nil", stage)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPromptResourceCleanupFailsClosedWithoutIdentityGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions are platform-specific on Windows")
	}
	root := t.TempDir()
	stagePath := filepath.Join(root, "stage")
	targetPath := filepath.Join(root, "target")
	if err := os.Mkdir(stagePath, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetPath, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, stagePath); err != nil {
		t.Fatal(err)
	}
	stage := &promptResourceStage{dir: stagePath}
	if err := stage.cleanup(); err == nil {
		t.Fatal("cleanup succeeded without retained identity guard")
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("replacement symlink target was removed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o500 {
		t.Fatalf("replacement symlink target mode = %o, want unchanged 500", got)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("guardless cleanup mutated replacement symlink: %v", err)
	}
}

func TestPromptResourceCleanupDoesNotInvokeHookWithoutIdentityGuard(t *testing.T) {
	dir := t.TempDir()
	stageDir := filepath.Join(dir, "stage")
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	stage := &promptResourceStage{
		dir: stageDir,
		cleanupHook: func(path string) error {
			called = true
			return nil
		},
	}
	if err := stage.cleanup(); err == nil {
		t.Fatal("cleanup succeeded without retained identity guard")
	}
	if stage.dir != stageDir {
		t.Fatalf("stage dir cleared after failed cleanup: %q", stage.dir)
	}
	if called {
		t.Fatal("guardless cleanup invoked named-path cleanup hook")
	}
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("guardless cleanup mutated stage: %v", err)
	}
}

func TestPreparePromptResourcesEnforcesConfiguredLimitsAndCleansPartialStage(t *testing.T) {
	encoded := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	tests := []struct {
		name   string
		limits PromptResourceLimits
		prompt []runtimeacp.ContentBlock
		want   string
	}{
		{
			name:   "file count",
			limits: PromptResourceLimits{MaxFiles: 1, MaxFileBytes: 8, MaxTotalBytes: 8},
			prompt: []runtimeacp.ContentBlock{{Type: "image", Data: encoded("a"), MimeType: "image/png"}, {Type: "image", Data: encoded("b"), MimeType: "image/png"}},
			want:   "more than 1",
		},
		{
			name:   "per file",
			limits: PromptResourceLimits{MaxFiles: 2, MaxFileBytes: 2, MaxTotalBytes: 8},
			prompt: []runtimeacp.ContentBlock{{Type: "image", Data: encoded("abc"), MimeType: "image/png"}},
			want:   "per-file limit",
		},
		{
			name:   "total",
			limits: PromptResourceLimits{MaxFiles: 2, MaxFileBytes: 3, MaxTotalBytes: 3},
			prompt: []runtimeacp.ContentBlock{{Type: "image", Data: encoded("ab"), MimeType: "image/png"}, {Type: "image", Data: encoded("cd"), MimeType: "image/png"}},
			want:   "total limit",
		},
		{
			name:   "invalid base64",
			limits: PromptResourceLimits{MaxFiles: 1, MaxFileBytes: 8, MaxTotalBytes: 8},
			prompt: []runtimeacp.ContentBlock{{Type: "image", Data: "%%%", MimeType: "image/png"}},
			want:   "not valid base64",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			_, stage, err := preparePromptResources(context.Background(), runtimeacp.PromptParams{Prompt: test.prompt}, test.limits, root, nil)
			if stage != nil {
				t.Fatalf("stage = %#v, want nil after failed preparation", stage)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("partial stage leaked after error: %#v", entries)
			}
		})
	}
}

func TestPreparationFailureAbandonsGuardsAfterBoundedCleanupFailure(t *testing.T) {
	root := t.TempDir()
	attempts := 0
	_, stage, err := preparePromptResources(context.Background(), runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("four")),
	}}}, PromptResourceLimits{MaxFiles: 1, MaxFileBytes: 4, MaxTotalBytes: 1}, root, func(string) error {
		attempts++
		return errors.New("persistent cleanup failure")
	})
	var cleanupErr *promptResourceCleanupError
	if stage != nil || !errors.As(err, &cleanupErr) {
		t.Fatalf("prepare result stage=%#v error=%v, want cleanup failure without returned ownership", stage, err)
	}
	if attempts != promptResourceCleanupAttempts {
		t.Fatalf("cleanup attempts = %d, want %d", attempts, promptResourceCleanupAttempts)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("protected remnants = %#v, want one abandoned anchor", entries)
	}
	// This production path returns no stage pointer. Successful manual removal
	// proves bounded failure closed retained POSIX descriptors/Windows handles.
	if err := os.RemoveAll(filepath.Join(root, entries[0].Name())); err != nil {
		t.Fatalf("remove abandoned preparation remnant after handles closed: %v", err)
	}
}

func TestPreparePromptResourcesRejectsRelativeTemporaryParent(t *testing.T) {
	_, stage, err := preparePromptResources(context.Background(), runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image")),
	}}}, PromptResourceLimits{}, "relative-temp", nil)
	var stagingErr *promptResourceStagingError
	if stage != nil || !errors.As(err, &stagingErr) || err.Error() != "validate prompt resource temporary parent failed" {
		t.Fatalf("prepare result stage=%#v err=%v, want scrubbed operational staging error", stage, err)
	}
}

func TestBridgeDistinguishesPromptValidationFromOperationalStagingFailures(t *testing.T) {
	tests := []struct {
		name     string
		tempDir  string
		data     string
		wantCode int
		wantMsg  string
	}{
		{name: "malformed prompt data", tempDir: t.TempDir(), data: "%%%", wantCode: -32602, wantMsg: "prepare prompt resources"},
		{name: "invalid staging configuration", tempDir: "relative-sensitive-parent", data: base64.StdEncoding.EncodeToString([]byte("image")), wantCode: -32000, wantMsg: "prompt resource staging failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := New(Spec{
				NewID:                 func() string { return "session-1" },
				PromptResourceTempDir: test.tempDir,
				BuildPrompt: func(Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
					return adapterprocess.Spec{Command: "agent"}, nil
				},
				Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
					return adapterprocess.Result{}, nil
				}),
			})
			client := acptest.NewClient(t, promptInputServer(bridge))
			client.Request("session/new", map[string]any{"cwd": t.TempDir()})
			responses := client.Send(map[string]any{
				"jsonrpc": "2.0",
				"id":      "prompt-1",
				"method":  "session/prompt",
				"params": map[string]any{
					"sessionId": "session-1",
					"prompt":    []map[string]any{{"type": "image", "mimeType": "image/png", "data": test.data}},
				},
			})
			final := responses[len(responses)-1]
			if final.Error == nil || final.Error.Code != test.wantCode || final.Error.Message != test.wantMsg {
				t.Fatalf("response = %#v, want %d/%q", final, test.wantCode, test.wantMsg)
			}
			raw, err := json.Marshal(final.Error)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), test.tempDir) || strings.Contains(string(raw), "must be absolute") {
				t.Fatalf("RPC error leaked staging configuration or raw OS detail: %s", raw)
			}
		})
	}
}

func TestFileURIPathValidationAcrossPlatforms(t *testing.T) {
	tests := []struct {
		name string
		goos string
		path string
		uri  string
	}{
		{name: "unix", goos: "linux", path: "/tmp/a b/file.txt", uri: "file:///tmp/a%20b/file.txt"},
		{name: "windows", goos: "windows", path: `C:\Users\A B\file.txt`, uri: "file:///C:/Users/A%20B/file.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fileURIFromPathForOS(test.path, test.goos); got != test.uri {
				t.Fatalf("file URI = %q, want %q", got, test.uri)
			}
			got, err := fileURIToPathForOS(test.uri, test.goos)
			if err != nil {
				t.Fatalf("fileURIToPathForOS returned error: %v", err)
			}
			if got != test.path {
				t.Fatalf("path = %q, want %q", got, test.path)
			}
		})
	}
}

func TestPromptResourcePathDirUsesTargetOSSemantics(t *testing.T) {
	tests := []struct {
		name      string
		localPath string
		goos      string
		want      string
	}{
		{name: "unix", localPath: "/tmp/private-stage/input.bin", goos: "linux", want: "/tmp/private-stage"},
		{name: "windows", localPath: `C:\Temp\private-stage\input.bin`, goos: "windows", want: `C:\Temp\private-stage`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := promptResourcePathDir(test.localPath, test.goos); got != test.want {
				t.Fatalf("promptResourcePathDir(%q, %q) = %q, want %q", test.localPath, test.goos, got, test.want)
			}
		})
	}
}

func TestPromptResourceStreamRedactorBoundsInvalidUTF8PendingBytes(t *testing.T) {
	alias := "/private/prompt-resource/alias"
	stream := (promptResourceRedactor{aliases: []string{alias}}).Stream()
	chunk := strings.Repeat(string([]byte{0x80}), 257)
	var output strings.Builder
	for index := 0; index < 128; index++ {
		output.WriteString(stream.Push(chunk))
		if got, max := len(stream.pending), len(alias)+utf8.UTFMax-2; got > max {
			t.Fatalf("pending invalid UTF-8 bytes = %d, want at most %d", got, max)
		}
	}
	output.WriteString(stream.Flush())
	want := strings.Repeat(chunk, 128)
	if output.String() != want {
		t.Fatalf("redacted invalid UTF-8 output length/content changed: got %d bytes, want %d", output.Len(), len(want))
	}
}

func TestPromptResourceStreamRedactorPreservesUTF8AcrossFirstBoundary(t *testing.T) {
	stream := (promptResourceRedactor{aliases: []string{"xxxx"}}).Stream()
	want := "éabc"
	chunks := []string{
		stream.Push("éab"),
		stream.Push("c"),
		stream.Flush(),
	}
	var output strings.Builder
	for index, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("output chunk %d split a valid UTF-8 rune: %x", index, []byte(chunk))
		}
		output.WriteString(chunk)
	}
	if got := output.String(); got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}

func TestRequirePromptTextFailsLoudWithoutDroppingRichBlocks(t *testing.T) {
	tests := []runtimeacp.ContentBlock{
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("image"))},
		{Type: "resource", Resource: &runtimeacp.EmbeddedResource{URI: "memory:///blob", Blob: base64.StdEncoding.EncodeToString([]byte("blob"))}},
		{Type: "resource_link", URI: "file:///tmp/source.txt"},
		{Type: "audio", Data: base64.StdEncoding.EncodeToString([]byte("audio"))},
		{Type: "text", Text: "visible", Data: base64.StdEncoding.EncodeToString([]byte("hidden"))},
	}
	for _, block := range tests {
		params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{Type: "text", Text: "do not silently return this"}, block}}
		if text := PromptText(params); text != "" {
			t.Fatalf("PromptText(%q) = %q, want fail-closed empty string", block.Type, text)
		}
		if text, err := RequirePromptText(params); err == nil || text != "" {
			t.Fatalf("RequirePromptText(%q) = %q, %v; want actionable error", block.Type, text, err)
		}
	}

	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{Type: "resource_link", URI: "https://example.invalid/reference"}}}
	text, err := RequirePromptText(params)
	if err != nil || !strings.Contains(text, "not fetched") || !strings.Contains(text, "https://example.invalid/reference") {
		t.Fatalf("non-file link text = %q, %v; want preserved reference", text, err)
	}
}

func TestPreparedFileMetadataIsNotSerializedOnACPWire(t *testing.T) {
	raw, err := json.Marshal(runtimeacp.ContentBlock{
		Type:         "text",
		Text:         "hello",
		PreparedFile: &runtimeacp.PreparedFile{Path: "/private/ephemeral/input.txt", OriginalURI: "memory:///private-source"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PreparedFile") || strings.Contains(string(raw), "/private/ephemeral") || strings.Contains(string(raw), "private-source") {
		t.Fatalf("prepared file metadata leaked onto ACP wire: %s", raw)
	}
}

func TestBridgeResourceStageCleansOnSuccessErrorAndCancellation(t *testing.T) {
	for _, outcome := range []string{"success", "runner_error", "builder_error"} {
		t.Run(outcome, func(t *testing.T) {
			var stageDir string
			bridge := New(Spec{
				NewID: func() string { return "session-1" },
				BuildPrompt: func(session Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
					inputs, err := PreparedPromptInputs(params)
					if err != nil {
						return adapterprocess.Spec{}, err
					}
					stageDir = filepath.Dir(inputs[1].Path)
					if len(session.AdditionalDirectories) != 2 || session.AdditionalDirectories[0] != "/existing" || session.AdditionalDirectories[1] != stageDir {
						t.Fatalf("additional dirs = %#v, want existing + private stage", session.AdditionalDirectories)
					}
					if outcome == "builder_error" {
						return adapterprocess.Spec{}, errors.New("builder failed")
					}
					return adapterprocess.Spec{Command: "agent", Args: []string{"private prompt"}}, nil
				},
				Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
					if outcome == "runner_error" {
						return adapterprocess.Result{}, errors.New("runner failed")
					}
					return adapterprocess.Result{}, nil
				}),
			})
			client := acptest.NewClient(t, promptInputServer(bridge))
			client.Request("session/new", map[string]any{"cwd": t.TempDir(), "additionalDirectories": []string{"/existing"}})
			responses := client.Send(richPromptRequest("prompt-1", "session-1"))
			if len(responses) == 0 {
				t.Fatalf("responses = %#v, want prompt lifecycle", responses)
			}
			if stageDir == "" {
				t.Fatal("builder did not observe stage")
			}
			if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage still exists after %s: %v", outcome, err)
			}
			if outcome == "success" {
				raw, err := json.Marshal(responses)
				if err != nil {
					t.Fatal(err)
				}
				serialized := string(raw)
				for _, forbidden := range []string{"private prompt", "private-name.png", stageDir} {
					if strings.Contains(serialized, forbidden) {
						t.Fatalf("ACP prompt tool activity leaked %q: %s", forbidden, serialized)
					}
				}
				if count := strings.Count(serialized, adapterprocess.RedactedValue); count != 2 {
					t.Fatalf("ACP prompt tool start/finish redaction count = %d, want 2: %s", count, serialized)
				}
				listed := client.Request("session/list", map[string]any{})
				var result struct {
					Sessions []struct {
						AdditionalDirectories []string `json:"additionalDirectories"`
					} `json:"sessions"`
				}
				listed.ResultInto(t, &result)
				if len(result.Sessions) != 1 || len(result.Sessions[0].AdditionalDirectories) != 1 || result.Sessions[0].AdditionalDirectories[0] != "/existing" {
					t.Fatalf("persistent session directories = %#v, want only /existing", result.Sessions)
				}
			}
		})
	}

	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		var stageDir string
		bridge := New(Spec{
			NewID: func() string { return "session-1" },
			BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
				inputs, err := PreparedPromptInputs(params)
				if err != nil {
					return adapterprocess.Spec{}, err
				}
				stageDir = filepath.Dir(inputs[1].Path)
				return adapterprocess.Spec{Command: "agent"}, nil
			},
			Runner: RunnerFunc(func(ctx context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
				close(started)
				<-ctx.Done()
				return adapterprocess.Result{}, ctx.Err()
			}),
		})
		client := acptest.NewClient(t, promptInputServer(bridge))
		client.Request("session/new", map[string]any{"cwd": t.TempDir()})
		done := make(chan []acptest.Response, 1)
		go func() { done <- client.Send(richPromptRequest("prompt-1", "session-1")) }()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("runner did not start")
		}
		client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled prompt did not settle")
		}
		if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage still exists after cancellation: %v", err)
		}
	})
}

func TestBridgeFailsClosedWhenPromptResourceCleanupFails(t *testing.T) {
	var stageDir string
	var sessions []Session
	var builtPrompts []runtimeacp.PromptParams
	cleanupAttempts := 0
	bridge := New(Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(session Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			inputs, err := PreparedPromptInputs(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			sessions = append(sessions, session)
			builtPrompts = append(builtPrompts, params)
			for _, input := range inputs {
				if input.Path != "" {
					stageDir = filepath.Dir(input.Path)
				}
			}
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("answer that must not be recorded before cleanup")}, nil
		}),
	})
	bridge.promptResourceCleanupHook = func(string) error {
		cleanupAttempts++
		return errors.New("injected cleanup failure")
	}
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	responses := client.Send(richPromptRequest("prompt-1", "session-1"))
	if len(responses) == 0 {
		t.Fatal("prompt returned no responses")
	}
	final := responses[len(responses)-1]
	if final.Error == nil || final.Error.Code != -32000 || final.Error.Message != "prompt resource cleanup failed" {
		t.Fatalf("final response = %#v, want cleanup failure", final)
	}
	if raw, _ := json.Marshal(final.Error.Data); strings.Contains(string(raw), stageDir) {
		t.Fatalf("cleanup response leaked ephemeral path: %s", raw)
	}
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("failed cleanup did not leave a protected remnant: %v", err)
	}
	if cleanupAttempts != promptResourceCleanupAttempts {
		t.Fatalf("cleanup attempts = %d, want bounded %d", cleanupAttempts, promptResourceCleanupAttempts)
	}
	client.Send(map[string]any{
		"jsonrpc": "2.0", "id": "prompt-2", "method": "session/prompt",
		"params": map[string]any{"sessionId": "session-1", "prompt": []map[string]any{{"type": "text", "text": "follow-up after failed cleanup"}}},
	})
	if len(sessions) != 2 || sessions[1].PromptCount != 0 {
		t.Fatalf("builder sessions = %#v, want failed-cleanup prompt excluded from count", sessions)
	}
	if len(builtPrompts) != 2 || len(builtPrompts[1].Prompt) != 1 ||
		builtPrompts[1].Prompt[0].Text != "follow-up after failed cleanup" ||
		strings.Contains(builtPrompts[1].Prompt[0].Text, "Previous conversation") {
		t.Fatalf("follow-up params = %#v, want no transcript from failed-cleanup prompt", builtPrompts)
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		t.Fatalf("make abandoned protected remnant removable: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(stageDir)); err != nil {
		t.Fatalf("manually remove abandoned protected remnant: %v", err)
	}
}

func TestBridgeVerifiesPromptStageImmediatelyBeforeBuilderAndLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("permission-drift injection is POSIX-specific")
	}
	for _, boundary := range []string{"builder", "launch"} {
		t.Run(boundary, func(t *testing.T) {
			builderCalls := 0
			runnerCalls := 0
			bridge := New(Spec{
				NewID: func() string { return "session-1" },
				BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
					builderCalls++
					inputs, err := PreparedPromptInputs(params)
					if err != nil {
						return adapterprocess.Spec{}, err
					}
					if boundary == "launch" {
						if err := os.Chmod(filepath.Dir(inputs[1].Path), 0o777); err != nil {
							return adapterprocess.Spec{}, err
						}
					}
					return adapterprocess.Spec{Command: "agent"}, nil
				},
				Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
					runnerCalls++
					return adapterprocess.Result{}, nil
				}),
			})
			if boundary == "builder" {
				bridge.promptStagePrepared = func(stage *promptResourceStage) {
					if err := os.Chmod(stage.dir, 0o777); err != nil {
						t.Fatalf("inject stage permission drift: %v", err)
					}
				}
			}
			client := acptest.NewClient(t, promptInputServer(bridge))
			client.Request("session/new", map[string]any{"cwd": t.TempDir()})
			responses := client.Send(richPromptRequest("prompt-1", "session-1"))
			final := responses[len(responses)-1]
			if final.Error == nil || final.Error.Message != "prompt resource stage verification failed" {
				t.Fatalf("final response = %#v, want stage verification failure", final)
			}
			wantBuilderCalls := 1
			if boundary == "builder" {
				wantBuilderCalls = 0
			}
			if builderCalls != wantBuilderCalls || runnerCalls != 0 {
				t.Fatalf("builder calls=%d runner calls=%d, want %d and 0", builderCalls, runnerCalls, wantBuilderCalls)
			}
		})
	}
}

func TestBridgeCancellationDuringCleanupDoesNotCommitPromptState(t *testing.T) {
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var cleanupOnce sync.Once
	var builderSessions []Session
	var builderPrompts []runtimeacp.PromptParams
	bridge := New(Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(session Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			builderSessions = append(builderSessions, session)
			builderPrompts = append(builderPrompts, params)
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("answer")}, nil
		}),
	})
	bridge.promptResourceCleanupHook = func(path string) error {
		cleanupOnce.Do(func() { close(cleanupStarted) })
		<-releaseCleanup
		return nil
	}
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	done := make(chan []acptest.Response, 1)
	go func() { done <- client.Send(richPromptRequest("prompt-1", "session-1")) }()
	select {
	case <-cleanupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not reach cleanup")
	}
	client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})
	close(releaseCleanup)
	var responses []acptest.Response
	select {
	case responses = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled prompt did not settle after cleanup")
	}
	var result runtimeacp.PromptResult
	responses[len(responses)-1].ResultInto(t, &result)
	if result.StopReason != runtimeacp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
	client.Send(map[string]any{
		"jsonrpc": "2.0", "id": "prompt-2", "method": "session/prompt",
		"params": map[string]any{"sessionId": "session-1", "prompt": []map[string]any{{"type": "text", "text": "follow-up"}}},
	})
	if len(builderSessions) != 2 || builderSessions[1].PromptCount != 0 {
		t.Fatalf("builder sessions = %#v, want cancelled prompt excluded from count", builderSessions)
	}
	if len(builderPrompts) != 2 || len(builderPrompts[1].Prompt) != 1 || strings.Contains(builderPrompts[1].Prompt[0].Text, "Previous conversation") {
		t.Fatalf("follow-up prompt = %#v, want no transcript from cancelled prompt", builderPrompts)
	}
}

func TestSessionCancelSettlesPendingPermissionAndCleansRichStage(t *testing.T) {
	var stageDir string
	var permissionID json.RawMessage
	bridge := New(Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(Session, runtimeacp.PromptParams) StreamParser {
			return NewJSONLStreamParser(func(event map[string]any) (JSONLMapping, error) {
				if event["type"] == "permission" {
					return JSONLMapping{Events: []StreamEvent{
						ToolCallPermissionRequest("tool-1", "Read attachment", "read", nil, nil),
					}}, nil
				}
				return JSONLMapping{}, nil
			})
		},
		BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			inputs, err := PreparedPromptInputs(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			stageDir = filepath.Dir(inputs[1].Path)
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: promptInputStreamingRunnerFunc(func(ctx context.Context, _ adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			err := onStdout([]byte("{\"type\":\"permission\"}\n"))
			if err == nil {
				return adapterprocess.Result{}, errors.New("permission callback unexpectedly completed")
			}
			<-ctx.Done()
			return adapterprocess.Result{}, ctx.Err()
		}),
	})
	client := acptest.NewLiveClient(t, promptInputServer(bridge), acptest.WithLiveResponseHandler(func(client *acptest.LiveClient, response acptest.Response) {
		if response.Method == "session/request_permission" {
			permissionID = append(json.RawMessage(nil), response.ID...)
			client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})
		}
	}))
	client.Request("new", "session/new", map[string]any{"cwd": t.TempDir()}, 2*time.Second)
	responses := client.Request("prompt-1", "session/prompt", richPromptRequest("ignored", "session-1")["params"], 2*time.Second)
	if len(permissionID) == 0 {
		t.Fatalf("responses = %#v, want pending permission request", responses)
	}
	var result runtimeacp.PromptResult
	for _, response := range responses {
		if acptest.ResponseIDEquals(response.ID, "prompt-1") && response.Method == "" {
			response.ResultInto(t, &result)
		}
	}
	if result.StopReason != runtimeacp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
	if stageDir == "" {
		t.Fatal("builder did not observe rich prompt stage")
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage still exists after cancellation: %v", err)
	}
	client.WritePermissionOutcome(permissionID, "selected", "allow_once")
}

func TestBridgeScrubsPromptStageAliasesFromBuilderAndRunnerFailures(t *testing.T) {
	for _, outcome := range []string{"builder", "runner"} {
		t.Run(outcome, func(t *testing.T) {
			var stageDir string
			var stagedPath string
			bridge := New(Spec{
				NewID: func() string { return "session-1" },
				BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
					inputs, err := PreparedPromptInputs(params)
					if err != nil {
						return adapterprocess.Spec{}, err
					}
					stagedPath = inputs[1].Path
					stageDir = filepath.Dir(stagedPath)
					anchorDir := filepath.Dir(stageDir)
					aliases := []string{anchorDir, filepath.Base(anchorDir), fileURIFromPath(anchorDir), stageDir, filepath.Base(stageDir), stagedPath, fileURIFromPath(stagedPath)}
					if outcome == "builder" {
						return adapterprocess.Spec{}, errors.New("builder exposed " + strings.Join(aliases, " | "))
					}
					return adapterprocess.Spec{Command: stagedPath, Dir: stageDir}, nil
				},
				Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
					anchorDir := filepath.Dir(stageDir)
					aliases := []string{anchorDir, filepath.Base(anchorDir), fileURIFromPath(anchorDir), stageDir, filepath.Base(stageDir), stagedPath, fileURIFromPath(stagedPath)}
					secret := strings.Join(aliases, " | ")
					return adapterprocess.Result{Stdout: []byte("stdout " + secret), Stderr: []byte("stderr " + secret)}, errors.New("runner exposed " + secret)
				}),
			})
			client := acptest.NewClient(t, promptInputServer(bridge))
			client.Request("session/new", map[string]any{"cwd": t.TempDir()})
			responses := client.Send(richPromptRequest("prompt-1", "session-1"))
			raw, err := json.Marshal(responses)
			if err != nil {
				t.Fatal(err)
			}
			anchorDir := filepath.Dir(stageDir)
			for _, alias := range []string{anchorDir, fileURIFromPath(anchorDir), stageDir, stagedPath, fileURIFromPath(stagedPath)} {
				if alias != "" && strings.Contains(string(raw), alias) {
					t.Fatalf("%s failure leaked prompt-stage alias %q: %s", outcome, alias, raw)
				}
			}
			for _, ordinaryText := range []string{filepath.Base(anchorDir), filepath.Base(stageDir)} {
				if !strings.Contains(string(raw), ordinaryText) {
					t.Fatalf("%s failure over-redacted basename-like operator text %q: %s", outcome, ordinaryText, raw)
				}
			}
			if !strings.Contains(string(raw), "[prompt-resource]") {
				t.Fatalf("%s failure did not contain redaction marker: %s", outcome, raw)
			}
			if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage remains after %s failure: %v", outcome, err)
			}
		})
	}
}

func TestBridgeStreamingRedactorCoversAliasSplitAcrossChunks(t *testing.T) {
	var stagedPath string
	var stageDir string
	bridge := New(Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			inputs, err := PreparedPromptInputs(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			stagedPath = inputs[1].Path
			stageDir = filepath.Dir(stagedPath)
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: promptInputStreamingRunnerFunc(func(_ context.Context, _ adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			split := len(stagedPath) / 2
			first := "before " + stagedPath[:split]
			second := stagedPath[split:] + " after"
			if err := onStdout([]byte(first)); err != nil {
				return adapterprocess.Result{}, err
			}
			if err := onStdout([]byte(second)); err != nil {
				return adapterprocess.Result{}, err
			}
			return adapterprocess.Result{Stdout: []byte(first + second)}, nil
		}),
	})
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	responses := client.Send(richPromptRequest("prompt-1", "session-1"))
	raw, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{stageDir, filepath.Base(stageDir), stagedPath, filepath.Base(stagedPath), fileURIFromPath(stagedPath)} {
		if alias != "" && strings.Contains(string(raw), alias) {
			t.Fatalf("split stream leaked alias %q: %s", alias, raw)
		}
	}
	if !strings.Contains(string(raw), "before [prompt-resource] after") {
		t.Fatalf("split stream lost redacted output boundary: %s", raw)
	}
}

func TestPromptResourceRedactorCoversWindowsVariantsAndSplitStreams(t *testing.T) {
	stageDir := `C:\Users\Ålice\AppData\Local\Temp\acp-commandbridge-private-AbCd\inputs-EfGh`
	stagedPath := stageDir + `\01-Secret File.PNG`
	params := runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type: "image",
		PreparedFile: &runtimeacp.PreparedFile{
			Path: stagedPath,
		},
	}}}
	redactor := newPromptResourceRedactorForOS(params, stageDir, "windows")
	anchor := promptResourcePathDir(stageDir, "windows")
	variants := []string{
		strings.ToLower(stagedPath),
		strings.ReplaceAll(stagedPath, `\`, "/"),
		strings.Replace(stagedPath, `\`, "/", 3),
		strings.ReplaceAll(stagedPath, `\`, `\\`),
		strings.ToUpper(fileURIFromPathForOS(stagedPath, "windows")),
		anchor,
		fileURIFromPathForOS(anchor, "windows"),
	}
	for _, variant := range variants {
		if got := redactor.Redact("before " + variant + " after"); strings.Contains(strings.ToLower(got), strings.ToLower(variant)) || !strings.Contains(got, "[prompt-resource]") {
			t.Fatalf("Redact(%q) = %q, want marker without alias", variant, got)
		}
		stream := redactor.Stream()
		split := len(variant) / 2
		got := stream.Push("before "+variant[:split]) + stream.Push(variant[split:]+" after") + stream.Flush()
		if strings.Contains(strings.ToLower(got), strings.ToLower(variant)) || !strings.Contains(got, "[prompt-resource]") {
			t.Fatalf("split redaction for %q = %q", variant, got)
		}
	}
	attachmentName := filepath.Base(strings.ReplaceAll(stagedPath, `\`, "/"))
	if got := redactor.Redact("attachment " + attachmentName); got != "attachment "+attachmentName {
		t.Fatalf("intentional attachment name was over-redacted: %q", got)
	}
	if got := redactor.RedactFinal(attachmentName + " completed"); got != attachmentName+" completed" {
		t.Fatalf("final attachment name was over-redacted as a path fragment: %q", got)
	}
}

func TestPromptResourceRedactorScrubsMeaningfulFinalBoundaryFragments(t *testing.T) {
	alias := "/private/acp-commandbridge-private-AbCd/inputs-EfGh/01-secret-document.png"
	redactor := promptResourceRedactor{aliases: []string{alias}}
	trailingFragment := alias[:len(alias)-4]
	leadingFragment := alias[4:]
	if got := redactor.RedactFinal("stdout " + trailingFragment); strings.Contains(got, trailingFragment) || !strings.Contains(got, "[prompt-resource]") {
		t.Fatalf("trailing fragment redaction = %q", got)
	}
	if got := redactor.RedactFinal(leadingFragment + " stderr"); strings.Contains(got, leadingFragment) || !strings.Contains(got, "[prompt-resource]") {
		t.Fatalf("leading fragment redaction = %q", got)
	}
	shortFragment := alias[:minPromptResourceBoundaryFragmentBytes-1]
	if got := redactor.RedactFinal("ordinary " + shortFragment); got != "ordinary "+shortFragment {
		t.Fatalf("short boundary fragment was over-redacted: %q", got)
	}
	stream := redactor.Stream()
	if got := stream.Push(trailingFragment); got != "" {
		t.Fatalf("stream emitted unresolved trailing fragment: %q", got)
	}
	if got := stream.Flush(); strings.Contains(got, trailingFragment) || !strings.Contains(got, "[prompt-resource]") {
		t.Fatalf("stream final fragment redaction = %q", got)
	}
	result := redactPromptCommandResult(adapterprocess.Result{
		Stdout: []byte("prefix " + trailingFragment),
		Stderr: []byte(leadingFragment + " suffix"),
	}, redactor.RedactFinal)
	if strings.Contains(string(result.Stdout), trailingFragment) || strings.Contains(string(result.Stderr), leadingFragment) {
		t.Fatalf("capped result leaked final fragments: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}

func TestRedactPromptStreamEventsCoversTypedContainersKeysAndRawJSON(t *testing.T) {
	const privatePath = "/private/acp-commandbridge-prompt-123/01-input.bin"
	redactor := promptResourceRedactor{aliases: []string{privatePath}}
	type structuredOutput struct {
		Path string `json:"path"`
	}
	events := []StreamEvent{{Update: map[string]any{
		privatePath: []map[string]any{{
			"typed": structuredOutput{Path: privatePath},
			"raw":   json.RawMessage(`{"path":"` + privatePath + `"}`),
		}},
	}}}

	redacted := redactPromptStreamEvents(events, redactor.RedactFinal)
	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privatePath) {
		t.Fatalf("structured stream update leaked private path: %s", raw)
	}
	if strings.Count(string(raw), "[prompt-resource]") < 3 {
		t.Fatalf("structured stream update lost redaction markers: %s", raw)
	}
}

func TestRedactPromptStreamEventsScrubsAliasFragmentsAcrossStructuredFields(t *testing.T) {
	const privatePath = "/private/acp-commandbridge-private-123/inputs-456/01-input.bin"
	redactor := promptResourceRedactor{aliases: []string{privatePath}}
	split := len(privatePath) / 2
	prefix, suffix := privatePath[:split], privatePath[split:]
	events := []StreamEvent{{
		Update: map[string]any{
			"title":  prefix,
			"detail": suffix,
		},
		PermissionRequest: &PermissionRequest{
			ToolCallID: prefix,
			Title:      suffix,
			RawInput: map[string]any{
				"prefix": prefix,
				"suffix": suffix,
			},
			Options: []PermissionOption{{OptionID: prefix, Name: suffix, Kind: "allow_once"}},
		},
	}}

	redacted := redactPromptStreamEvents(events, redactor.RedactFinal)
	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), prefix) || strings.Contains(string(raw), suffix) {
		t.Fatalf("structured stream fields leaked path fragments: %s", raw)
	}
	if strings.Count(string(raw), "[prompt-resource]") < 8 {
		t.Fatalf("structured stream fields lost redaction markers: %s", raw)
	}
}

func TestBridgeAlreadyCancelledPromptSkipsBuilderAndRunner(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var built, ran bool
	bridge := New(Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			built = true
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			ran = true
			return adapterprocess.Result{}, nil
		}),
	})
	bridge.promptMethodContext = func(*acp.MethodContext) context.Context { return cancelledCtx }
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0", "id": "prompt-1", "method": "session/prompt",
		"params": map[string]any{"sessionId": "session-1", "prompt": []map[string]any{{"type": "text", "text": "do not run"}}},
	})
	if built || ran {
		t.Fatalf("cancelled prompt built=%v ran=%v, want both false", built, ran)
	}
	if len(responses) != 1 {
		t.Fatalf("responses = %#v, want only cancelled result", responses)
	}
	var result runtimeacp.PromptResult
	responses[0].ResultInto(t, &result)
	if result.StopReason != runtimeacp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
}

func TestBridgeCancellationAfterPreparationSkipsRunnerAndCleansStage(t *testing.T) {
	var bridge *Bridge
	var stageDir string
	var ran bool
	bridge = New(Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(session Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			inputs, err := PreparedPromptInputs(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			stageDir = filepath.Dir(inputs[1].Path)
			if !bridge.cancel(session.ID) {
				t.Fatal("active prompt was not cancellable from builder")
			}
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			ran = true
			return adapterprocess.Result{}, nil
		}),
	})
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	responses := client.Send(richPromptRequest("prompt-1", "session-1"))
	if ran {
		t.Fatal("runner was called after cancellation in BuildPrompt")
	}
	if len(responses) != 1 {
		t.Fatalf("responses = %#v, want only cancelled result", responses)
	}
	var result runtimeacp.PromptResult
	responses[0].ResultInto(t, &result)
	if result.StopReason != runtimeacp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage still exists after pre-run cancellation: %v", err)
	}
}

func TestPreparePromptResourcesRejectsCancelledContextWithoutCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, stage, err := preparePromptResources(ctx, runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{
		{Type: "text", Text: "do not prepare"},
		{Type: "resource_link", URI: "https://example.invalid/reference", Name: "reference"},
	}}, PromptResourceLimits{}, t.TempDir(), nil)
	if !errors.Is(err, context.Canceled) || stage != nil {
		t.Fatalf("prepare result stage=%#v err=%v, want context cancellation without stage", stage, err)
	}
}

func TestBridgeTranscriptPreservesCurrentResourceBoundaryWithoutEphemeralHistory(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "transcript-secret-name.txt")
	if err := os.WriteFile(sourcePath, []byte("attached body must not enter transcript"), 0o600); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	var preparedTurns [][]PreparedPromptInput
	var firstStagePath string
	var runCount int
	bridge := New(Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(_ Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			inputs, inputErr := PreparedPromptInputs(params)
			if inputErr != nil {
				return adapterprocess.Spec{}, inputErr
			}
			preparedTurns = append(preparedTurns, inputs)
			for _, input := range inputs {
				if input.Path != "" && firstStagePath == "" {
					firstStagePath = input.Path
				}
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent", Args: []string{text}}, nil
		},
		Runner: RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			runCount++
			if runCount == 1 {
				resolved, _ := filepath.EvalSymlinks(firstStagePath)
				return adapterprocess.Result{Stdout: []byte("answer from " + firstStagePath + " and " + fileURIFromPath(firstStagePath) + " and " + resolved)}, nil
			}
			return adapterprocess.Result{Stdout: []byte("answer")}, nil
		}),
	})
	client := acptest.NewClient(t, promptInputServer(bridge))
	client.Request("session/new", map[string]any{"cwd": t.TempDir()})
	client.Send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
		"params": map[string]any{"sessionId": "session-1", "prompt": []map[string]any{
			{"type": "text", "text": "first request"},
			{"type": "resource", "resource": map[string]any{"uri": "memory:///current.txt", "text": "current embedded text", "mimeType": "text/plain"}},
			{"type": "resource_link", "uri": fileURIFromPath(sourcePath), "name": filepath.Base(sourcePath)},
		}},
	})
	client.Send(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{"sessionId": "session-1", "prompt": []map[string]any{
			{"type": "text", "text": "second request prefix"},
			{"type": "image", "mimeType": "image/png", "uri": "memory:///turn-two-image.png", "data": base64.StdEncoding.EncodeToString([]byte("turn-two-image-body"))},
			{"type": "text", "text": "second request suffix"},
		}},
	})
	if len(prompts) != 2 {
		t.Fatalf("prompts = %#v, want two", prompts)
	}
	if !strings.Contains(prompts[0], `"kind":"embedded_text"`) || !strings.Contains(prompts[0], `"text":"current embedded text"`) {
		t.Fatalf("first prompt lost embedded-resource boundary:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[1], "User:\nfirst request") || !strings.Contains(prompts[1], "Current user request:") ||
		!strings.Contains(prompts[1], "second request prefix") || !strings.Contains(prompts[1], "second request suffix") ||
		!strings.Contains(prompts[1], `"kind":"image"`) || !strings.Contains(prompts[1], "[prompt-resource]") {
		t.Fatalf("second prompt missing durable transcript/current resource:\n%s", prompts[1])
	}
	if strings.Count(prompts[1], "second request prefix") != 1 || strings.Count(prompts[1], "second request suffix") != 1 {
		t.Fatalf("second prompt duplicated current text blocks:\n%s", prompts[1])
	}
	if len(preparedTurns) != 2 || len(preparedTurns[1]) != 4 {
		t.Fatalf("prepared turns = %#v, want history + text/image/text", preparedTurns)
	}
	secondInputs := preparedTurns[1]
	if secondInputs[0].Kind != PreparedInputText || !strings.HasSuffix(secondInputs[0].Text, "Current user request:") ||
		secondInputs[1].Kind != PreparedInputText || secondInputs[1].Text != "second request prefix" ||
		secondInputs[2].Kind != PreparedInputImage || secondInputs[2].Name != "turn-two-image.png" ||
		secondInputs[3].Kind != PreparedInputText || secondInputs[3].Text != "second request suffix" {
		t.Fatalf("turn-two inputs = %#v, want exact history + text/image/text order", secondInputs)
	}
	resolvedStagePath, _ := filepath.EvalSymlinks(firstStagePath)
	for _, forbidden := range []string{"current embedded text", "attached body must not enter transcript", sourcePath, firstStagePath, resolvedStagePath, filepath.Base(sourcePath)} {
		if forbidden != "" && strings.Contains(prompts[1], forbidden) {
			t.Fatalf("second prompt retained ephemeral attachment data %q:\n%s", forbidden, prompts[1])
		}
	}
}

func TestPromptParamsForSessionPrependsHistoryForFileOnlyFollowUp(t *testing.T) {
	bridge := New(Spec{IncludeTranscript: true})
	state := &sessionState{transcript: []transcriptExchange{{User: "first request", Assistant: "first answer"}}}
	current := runtimeacp.ContentBlock{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image"))}
	params := bridge.promptParamsForSession(state, runtimeacp.PromptParams{
		SessionID: "session-1",
		Prompt:    []runtimeacp.ContentBlock{current},
	})
	if len(params.Prompt) != 2 {
		t.Fatalf("prompt blocks = %#v, want history prelude + current file", params.Prompt)
	}
	if params.Prompt[0].Type != "text" || !strings.Contains(params.Prompt[0].Text, "User:\nfirst request") ||
		!strings.HasSuffix(params.Prompt[0].Text, "Current user request:") {
		t.Fatalf("history prelude = %#v, want prior exchange and current boundary", params.Prompt[0])
	}
	if params.Prompt[1].Type != current.Type || params.Prompt[1].MimeType != current.MimeType || params.Prompt[1].Data != current.Data {
		t.Fatalf("current file block = %#v, want unchanged %#v", params.Prompt[1], current)
	}
}

func promptInputServer(bridge *Bridge) *acp.Server {
	return acp.NewServer(acp.AdapterInfo{Name: "prompt-input-test", Title: "Prompt Input Test"}, bridge.Options()...)
}

func richPromptRequest(id, sessionID string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": "private prompt"},
				{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("image")), "name": "private-name.png"},
			},
		},
	}
}

type promptInputStreamingRunnerFunc func(context.Context, adapterprocess.Spec, func([]byte) error) (adapterprocess.Result, error)

func (f promptInputStreamingRunnerFunc) Run(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
	return adapterprocess.Result{}, errors.New("buffered Run should not be called")
}

func (f promptInputStreamingRunnerFunc) RunStream(ctx context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
	return f(ctx, spec, onStdout)
}
