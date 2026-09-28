package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestCancellationClassesDoNotExposeErrorText(t *testing.T) {
	for _, row := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {fmt.Errorf("private payload: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"}, {errors.New("private credential"), "other_error"},
	} {
		if got := cancellationClass(row.err); got != row.want {
			t.Fatalf("got %s, want %s", got, row.want)
		}
	}
}

func TestCancellationTraceSeparatesClientCancellationFromHandlerErrorAfter200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := test.NewGlobal()
	defer func() { log.StandardLogger().ReplaceHooks(previousHooks) }()
	for _, clientCanceled := range []bool{true, false} {
		hook.Reset()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		requestCtx, requestCancel := context.WithCancel(logging.WithRequestID(context.Background(), "abcdef01"))
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
		c.Request.Header.Set("Authorization", "private credential")
		c.Request.Header.Set("User-Agent", "Codex/private-user-agent")
		c.Request.RemoteAddr = "127.0.0.1:23456"
		handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		ctx, finish := handler.GetContextWithCancel(nil, c, context.Background())
		// The watcher must use the immutable snapshot, not mutable Gin headers.
		c.Request.Header.Set("User-Agent", "curl/private-mutated-agent")
		c.Writer.WriteHeaderNow()
		if clientCanceled {
			requestCancel()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("request cancellation did not propagate")
			}
			finish(context.Canceled)
		} else {
			finish(errors.New("private error body"))
			requestCancel()
		}
		finish(nil)
		returns, clientEvents := 0, 0
		for _, entry := range hook.AllEntries() {
			if !strings.HasPrefix(entry.Message, "request_cancel_trace ") {
				continue
			}
			if strings.Contains(fmt.Sprint(entry.Data), "private") {
				t.Fatal("private contents in trace")
			}
			if entry.Data["client_family"] != "codex" || entry.Data["peer_class"] != "loopback" || entry.Data["request_deadline"] != "none" || entry.Data["parent_deadline"] != "none" {
				t.Fatalf("origin snapshot changed: %v", entry.Data)
			}
			formatted, formatErr := (&logging.LogFormatter{}).Format(entry)
			if formatErr != nil || !strings.Contains(string(formatted), "[abcdef01]") || !strings.Contains(string(formatted), "event="+fmt.Sprint(entry.Data["event"])) || !strings.Contains(string(formatted), "elapsed_ms=") || strings.Contains(string(formatted), "private") {
				t.Fatalf("production formatter lost diagnostic fields: %s (%v)", formatted, formatErr)
			}
			if !strings.Contains(string(formatted), "client_family=codex peer_class=loopback request_deadline=none parent_deadline=none") || strings.Contains(string(formatted), "127.0.0.1") {
				t.Fatalf("origin lost or raw metadata leaked: %s", formatted)
			}
			if entry.Data["event"] == "request_context_done" {
				clientEvents++
			}
			if entry.Data["event"] == "handler_return" {
				returns++
				want := "other_error"
				if clientCanceled {
					want = "canceled"
				}
				if entry.Data["reason"] != want || entry.Data["response_status"] != 200 || entry.Data["headers_committed"] != true {
					t.Fatalf("unexpected terminal fields: %v", entry.Data)
				}
				if !strings.Contains(string(formatted), "response_status=200 headers_committed=true") || !strings.Contains(string(formatted), "reason="+want) {
					t.Fatalf("missing committed response or reason in production log: %s", formatted)
				}
			}
		}
		if returns != 1 || (clientCanceled && clientEvents != 1) {
			t.Fatalf("returns=%d request events=%d", returns, clientEvents)
		}
	}
}

func TestCancellationOriginClassifiersBoundValues(t *testing.T) {
	for _, row := range []struct{ value, want string }{
		{"", "missing"}, {"OpenAI-Python/1.0 private", "openai_python"},
		{"OpenAI/Node 1.0", "openai_node"}, {"FractalBot private", "fractalbot"},
		{"python-urllib/3", "python_urllib"}, {"python-requests/2", "python_requests"},
		{"Go-http-client/1.1", "go_http"}, {"curl/8.0", "curl"},
		{"arbitrary-private-agent", "other"}, {strings.Repeat("x", 300) + "codex", "other"},
	} {
		if got := clientFamily(row.value); got != row.want {
			t.Fatalf("family got %s want %s", got, row.want)
		}
	}
	for _, row := range []struct{ value, want string }{
		{"", "missing"}, {"private-host-name", "non_ip"}, {"127.0.0.1", "loopback"},
		{"::1", "loopback"}, {"::ffff:127.0.0.1", "loopback"}, {"10.0.0.1", "private"},
		{"fe80::1", "private"}, {"192.0.2.1", "other_ip"},
	} {
		if got := peerClass(row.value); got != row.want {
			t.Fatalf("peer got %s want %s", got, row.want)
		}
	}
	at := time.Now()
	for _, row := range []struct {
		remaining time.Duration
		want      string
	}{
		{-time.Second, "expired"}, {5 * time.Second, "le_5s"},
		{30 * time.Second, "le_30s"}, {120 * time.Second, "le_120s"},
		{121 * time.Second, "gt_120s"},
	} {
		ctx, cancel := context.WithDeadline(context.Background(), at.Add(row.remaining))
		got := deadlineClass(ctx, at)
		cancel()
		if got != row.want {
			t.Fatalf("deadline got %s want %s", got, row.want)
		}
	}
	if deadlineClass(nil, at) != "none" || deadlineClass(context.Background(), at) != "none" {
		t.Fatal("missing deadline must not be inferred")
	}
}

// net/http preserves IPv6 interface zones in RemoteAddr. Classify the host
// returned by requestClientIP; do not mistake a scoped IP for a caller name.
func TestCancellationOriginScopedRemoteAddr(t *testing.T) {
	for _, row := range []struct{ remote, want string }{
		{"[fe80::1%fixture-interface]:43123", "private"},
		{"[fd00::1%fixture-interface]:43123", "private"},
		{"[::1%fixture-interface]:43123", "loopback"},
		{"[::ffff:127.0.0.1]:43123", "loopback"},
		{"[::ffff:10.0.0.1%fixture-interface]:43123", "private"},
		{"[2001:db8::1%fixture-interface]:43123", "other_ip"},
		{"private-host-name:43123", "non_ip"},
		{"127.0.0.1%fixture-interface:43123", "non_ip"},
		{"[fe80::bad::1%fixture-interface]:43123", "non_ip"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req.RemoteAddr = row.remote
		if got := peerClass(requestClientIP(req)); got != row.want {
			t.Errorf("scoped peer class got %s want %s", got, row.want)
		}
	}
}

func TestScopedCancellationOriginKeepsRawPeerOutOfFormattedLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := test.NewGlobal()
	defer func() { log.StandardLogger().ReplaceHooks(previousHooks) }()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(
		logging.WithRequestID(context.Background(), "abcd0123"))
	c.Request.RemoteAddr = "[fe80::1%fixture-private-interface]:43123"
	c.Request.Header.Set("User-Agent", "Codex/fixture-private-agent")
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	_, finish := h.GetContextWithCancel(nil, c, c.Request.Context())
	finish(context.Canceled)
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("expected one terminal log, got %d", len(entries))
	}
	formatted, err := (&logging.LogFormatter{}).Format(entries[0])
	if err != nil || !strings.Contains(string(formatted), "client_family=codex peer_class=private ") {
		t.Fatal("scoped classification missing from production formatter")
	}
	for _, forbidden := range []string{"fe80::", "fixture-private-interface", "fixture-private-agent", "43123"} {
		if strings.Contains(string(formatted), forbidden) || strings.Contains(fmt.Sprint(entries[0].Data), forbidden) {
			t.Fatal("raw client metadata leaked into cancellation log")
		}
	}
}

func TestCancellationOriginSeparatesRequestAndParentDeadlines(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer requestCancel()
	parentCtx, parentCancel := context.WithTimeout(context.Background(), time.Minute)
	defer parentCancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, finish := h.GetContextWithCancel(nil, c, parentCtx)
	defer finish(nil)
	origin := ctx.Value(cancellationOriginKey{}).(cancellationOrigin)
	if origin.requestDeadline != "le_5s" || origin.parentDeadline != "le_120s" {
		t.Fatalf("independent deadline budgets lost: %+v", origin)
	}
}
