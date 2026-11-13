package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// affinityRecordingExecutor records which credential served each websocket turn.
type affinityRecordingExecutor struct {
	mu      sync.Mutex
	authIDs []string
}

func (*affinityRecordingExecutor) Identifier() string { return "codex" }

func (*affinityRecordingExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *affinityRecordingExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-affinity","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*affinityRecordingExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (*affinityRecordingExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*affinityRecordingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *affinityRecordingExecutor) takeAuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := e.authIDs
	e.authIDs = nil
	return ids
}

// TestResponsesWebsocketSessionAffinitySurvivesReconnect verifies that a Codex
// thread keeps one credential across turns on a socket and across reconnects,
// identified either by the session_id upgrade header or by prompt_cache_key.
func TestResponsesWebsocketSessionAffinitySurvivesReconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	model := "gpt-ws-affinity-test"
	authIDs := []string{"ws-affinity-a", "ws-affinity-b", "ws-affinity-c"}
	executor := &affinityRecordingExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	affinity := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: coreauth.SessionAffinityRebindOnQuota,
	})
	defer affinity.Stop()
	manager.SetSelector(affinity)
	manager.RegisterExecutor(executor)
	for _, id := range authIDs {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		authID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"

	runConnection := func(header http.Header, requests ...string) []string {
		t.Helper()
		conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, header)
		if errDial != nil {
			t.Fatalf("dial websocket: %v", errDial)
		}
		defer func() { _ = conn.Close() }()
		for index, request := range requests {
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
				t.Fatalf("write request %d: %v", index+1, errWrite)
			}
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("read response %d: %v", index+1, errRead)
			}
			if got := ojson.GetBytesNoCopy(payload, "type").String(); got != wsEventTypeCompleted {
				t.Fatalf("response %d type = %q: %s", index+1, got, payload)
			}
		}
		return executor.takeAuthIDs()
	}
	requireSingleAuth := func(label string, ids []string, want int) string {
		t.Helper()
		if len(ids) != want {
			t.Fatalf("%s: executed %d turns (%v), want %d", label, len(ids), ids, want)
		}
		for _, id := range ids {
			if id != ids[0] {
				t.Fatalf("%s: turns switched credentials: %v", label, ids)
			}
		}
		return ids[0]
	}

	turn := func(text string) string {
		return `{"type":"response.create","model":"` + model + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}]}`
	}
	cacheKeyTurn := func(key, text string) string {
		return `{"type":"response.create","model":"` + model + `","prompt_cache_key":"` + key + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}]}`
	}

	for _, testCase := range []struct {
		name    string
		header  func(session string) http.Header
		request func(session, text string) string
	}{
		{
			name:    "session_id header",
			header:  func(session string) http.Header { return http.Header{"Session_id": {session}} },
			request: func(_ string, text string) string { return turn(text) },
		},
		{
			name:    "prompt_cache_key",
			header:  func(string) http.Header { return nil },
			request: cacheKeyTurn,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			session := "thread-" + strings.ReplaceAll(testCase.name, " ", "-")
			first := requireSingleAuth("first connection", runConnection(testCase.header(session),
				testCase.request(session, "plan the change"),
				testCase.request(session, "apply it"),
			), 2)

			// Another thread starting in between advances the round-robin cursor,
			// so a reconnect that lost its binding would land elsewhere.
			other := requireSingleAuth("other thread", runConnection(testCase.header(session+"-other"),
				testCase.request(session+"-other", "unrelated"),
			), 1)
			if other == first {
				t.Fatalf("other thread reused %q; round-robin fallback did not rotate", first)
			}

			second := requireSingleAuth("reconnect", runConnection(testCase.header(session),
				testCase.request(session, "continue after reconnect"),
				testCase.request(session, "run the tests"),
			), 2)
			if second != first {
				t.Fatalf("reconnect switched credential from %q to %q", first, second)
			}
		})
	}
}
