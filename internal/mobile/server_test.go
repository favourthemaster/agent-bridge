package mobile

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/parser"
	"agent-bridge/internal/queue"
	"agent-bridge/internal/runner"

	"github.com/gorilla/websocket"
)

func setupTestMobileServer(t *testing.T, authToken string) (*Server, *runner.FakeRunner, *httptest.Server, string) {
	t.Helper()
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "demo-proj")
	if err := os.MkdirAll(filepath.Join(projDir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(projDir, "README.md"), []byte("# Demo Project\nOverview text."), 0644)
	_ = os.WriteFile(filepath.Join(projDir, "docs", "architecture.md"), []byte("# Architecture Plan"), 0644)
	_ = os.WriteFile(filepath.Join(projDir, ".env"), []byte("SECRET_KEY=topsecret"), 0644)

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := &config.Config{
		Mobile: config.MobileConfig{
			Enabled:    true,
			ListenAddr: ":0",
			AuthToken:  authToken,
		},
		Projects: map[string]config.ProjectConfig{
			"demo": {
				Path:         projDir,
				DefaultModel: "gemini-3.8-flash-high",
				AutoApprove:  false,
			},
		},
		Models: []config.ModelConfig{
			{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		},
		Limits: config.LimitsConfig{
			MaxFileSizeMB:          10,
			ApprovalTimeoutMinutes: 5,
		},
		Defaults: config.DefaultsConfig{
			Project:     "demo",
			Verbosity:   "normal",
			AutoApprove: false,
		},
	}

	fake := runner.NewFakeRunner()
	fanout := runner.NewFanoutRunner(fake)
	q := queue.NewManager(database, fanout, "")

	srv, err := NewServer(cfg, database, fanout, q)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, fake, ts, projDir
}

func TestAuthAndWorkspaceSandbox(t *testing.T) {
	_, _, ts, _ := setupTestMobileServer(t, "secret-token-123")

	// Unauthenticated request should fail with 401
	resp, err := http.Get(ts.URL + "/api/v1/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	// Authenticated artifacts request should succeed
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/artifacts", nil)
	req.Header.Set("Authorization", "Bearer secret-token-123")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	// Sensitive .env file must be blocked by path sandbox
	reqEnv, _ := http.NewRequest("GET", ts.URL+"/api/v1/files/content?path=.env", nil)
	reqEnv.Header.Set("Authorization", "Bearer secret-token-123")
	respEnv, err := http.DefaultClient.Do(reqEnv)
	if err != nil {
		t.Fatal(err)
	}
	respEnv.Body.Close()
	if respEnv.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for .env access, got %d", respEnv.StatusCode)
	}
}

func TestWebSocketStreamingAndHTTPApproval(t *testing.T) {
	_, fake, ts, _ := setupTestMobileServer(t, "tok99")

	fake.OnInstruction = func(text string, r *runner.FakeRunner) {
		r.EmitEvent(parser.ParsedEvent{Type: parser.EventThinkingChunk, Text: "Analyzing codebase..."})
		r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolStart, ID: "s1", Text: "read README.md"})
		r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolEnd, ID: "s1", Text: "read README.md", Ok: true, DurationMs: 25})
		r.EmitEvent(parser.ParsedEvent{Type: parser.EventApprovalReq, ID: "app_test_1", Text: "rm -rf dist/"})

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cnt, allowed := r.ApprovalSnapshot(); cnt > 0 {
				if allowed {
					r.EmitEvent(parser.ParsedEvent{Type: parser.EventAssistantChunk, Text: "Approved and cleaned!"})
				} else {
					r.EmitEvent(parser.ParsedEvent{Type: parser.EventAssistantChunk, Text: "Denied."})
				}
				r.EmitEvent(parser.ParsedEvent{Type: parser.EventPromptReady})
				r.SetBusy(false)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws?token=tok99"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.Close()

	// Expect initial hello
	var hello ServerMessage
	if err := conn.ReadJSON(&hello); err != nil {
		t.Fatalf("reading hello: %v", err)
	}
	if hello.Event != "hello" {
		t.Fatalf("expected hello event, got %s", hello.Event)
	}

	// Send prompt
	if err := conn.WriteJSON(ClientAction{Action: "send_prompt", Text: "Clean dist"}); err != nil {
		t.Fatal(err)
	}

	gotApproval := false
	gotComplete := false
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))

	for !gotComplete {
		var ev ServerMessage
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("ws read error before completion: %v", err)
		}
		if ev.Event == "approval_request" && !gotApproval {
			gotApproval = true
			// Resolve via HTTP POST just like Android background notification action button!
			payload := []byte(`{"id":"app_test_1","allowed":true}`)
			req, _ := http.NewRequest("POST", ts.URL+"/api/v1/approvals/resolve", bytes.NewReader(payload))
			req.Header.Set("Authorization", "Bearer tok99")
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("resolve HTTP status: %d", res.StatusCode)
			}
		}
		if ev.Event == "turn_complete" {
			gotComplete = true
		}
	}

	if !gotApproval {
		t.Fatalf("expected approval_request event")
	}
}

func TestMultipartVoiceUpload(t *testing.T) {
	_, _, ts, _ := setupTestMobileServer(t, "")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("is_voice", "true")
	fw, err := mw.CreateFormFile("file", "voice_note.m4a")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("fake-audio-bytes"))
	_ = mw.Close()

	resp, err := http.Post(ts.URL+"/api/v1/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}
	var ref AttachmentRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		t.Fatal(err)
	}
	if !ref.IsVoice || ref.Category != "audio" {
		t.Fatalf("unexpected voice ref: %+v", ref)
	}
}
