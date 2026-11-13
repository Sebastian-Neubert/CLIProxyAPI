package claude

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeCooldownReset(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			before := time.Now().Unix()
			NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)).WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: 429, Error: coreauth.NewModelCooldownError("claude-opus-5-5", "claude", time.Hour),
			})
			raw := w.Header().Get("Anthropic-Ratelimit-Unified-Reset")
			if path != "/v1/messages" {
				if raw != "" {
					t.Fatal("Claude headers on OpenAI route")
				}
				return
			}
			reset, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || reset < before+3600 || reset > time.Now().Unix()+3600 {
				t.Fatalf("missing/incorrect reset: %q", raw)
			}
			if w.Header().Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
				t.Fatal("missing rejected status")
			}
			if w.Header().Get("Anthropic-Ratelimit-Unified-Representative-Claim") != "unknown" {
				t.Fatal("must not invent quota window")
			}
		})
	}
}

func TestClaudeCooldownNoInventedReset(t *testing.T) {
	for _, delay := range []time.Duration{0, -time.Second} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 429, Error: coreauth.NewModelCooldownError("claude-opus-5-5", "claude", delay)})
		if w.Header().Get("Anthropic-Ratelimit-Unified-Reset") != "" {
			t.Fatal("invented reset for absent deadline")
		}
	}
}

// TestClaudeCooldownSDK drives a locally installed Claude Code CLI against a
// fixture server (no upstream traffic). It is opt-in because it depends on an
// external binary: set CLIPROXY_CLAUDE_CLI_TEST=1 to run it.
func TestClaudeCooldownSDK(t *testing.T) {
	if os.Getenv("CLIPROXY_CLAUDE_CLI_TEST") != "1" {
		t.Skip("set CLIPROXY_CLAUDE_CLI_TEST=1 to run the Claude CLI integration test")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude CLI required for SDK integration test")
	}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		handler.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 429, Error: coreauth.NewModelCooldownError("claude-opus-5-5", "claude", time.Hour)})
	}))
	defer server.Close()
	cmd := exec.Command(binary, "-p", "Reply pong", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--max-turns", "1", "--setting-sources", "", "--tools", "")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+server.URL, "ANTHROPIC_AUTH_TOKEN=fixture", "CLAUDE_CODE_USE_GATEWAY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	before := time.Now().Unix()
	output, _ := cmd.Output() // A rejected request intentionally exits nonzero.
	for _, line := range bytes.Split(output, []byte("\n")) {
		event := ojson.GetBytesNoCopy(line, "@this")
		reset := event.Get("rate_limit_info.resetsAt").Int()
		if event.Get("type").String() == "rate_limit_event" && event.Get("rate_limit_info.status").String() == "rejected" &&
			reset >= before+3600 && reset <= time.Now().Unix()+3600 {
			return
		}
	}
	t.Fatal("Claude SDK did not emit rejected rate_limit_event with exact proxy deadline")
}

func TestClaudeUpstreamRetryDeadline(t *testing.T) {
	for _, direct := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		before := time.Now().Unix()
		headers := http.Header{"Retry-After": {"3600"}}
		NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 429, DirectResponse: direct, Headers: headers, Addon: headers, Body: []byte(`{"error":{"type":"rate_limit_error"}}`)})
		reset, _ := strconv.ParseInt(w.Header().Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
		if reset < before+3600 || reset > time.Now().Unix()+3600 {
			t.Fatalf("direct=%v: missing reset", direct)
		}
	}
}

func TestClaudePreservesUpstreamReset(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	headers := http.Header{}
	headers.Set("Retry-After", "3600")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", "2000000000")
	headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 429, Addon: headers})
	if w.Header().Get("Anthropic-Ratelimit-Unified-Reset") != "2000000000" || w.Header().Get("Anthropic-Ratelimit-Unified-Representative-Claim") != "seven_day" {
		t.Fatal("overwrote actual upstream reset")
	}
}

func TestClaudeCooldownKeepsAlignedUnifiedWindows(t *testing.T) {
	for _, tt := range []struct {
		name      string
		resetIn   int64
		wantClaim string
	}{
		{name: "aligned with cooldown deadline", resetIn: 3600, wantClaim: "seven_day"},
		{name: "stale per-account window", resetIn: 60, wantClaim: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			before := time.Now().Unix()
			addon := http.Header{}
			addon.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			addon.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
			addon.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(before+tt.resetIn, 10))
			addon.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil))
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: http.StatusTooManyRequests,
				Error:      coreauth.NewModelCooldownError("claude-opus-5-5", "claude", time.Hour),
				Addon:      addon,
			})
			if got := w.Header().Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != tt.wantClaim {
				t.Fatalf("claim = %q, want %q", got, tt.wantClaim)
			}
			reset, _ := strconv.ParseInt(w.Header().Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
			if reset < before+3600-claudeResetSkewSeconds || reset > time.Now().Unix()+3600+claudeResetSkewSeconds {
				t.Fatalf("reset = %d, want cooldown deadline", reset)
			}
		})
	}
}

func TestClaudeRetryDeadlineRequiresPassthrough(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)).WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests, Error: coreauth.NewModelCooldownError("claude-opus-5-5", "claude", time.Hour),
	})
	if got := w.Header().Get("Anthropic-Ratelimit-Unified-Reset"); got != "" {
		t.Fatalf("unified reset emitted without passthrough headers: %q", got)
	}
}
