package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
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

const requestNonce = "12345678-1234-4234-8234-123456789abc"

func TestClientRequestMarkerRejectsRawMetadata(t *testing.T) {
	for _, values := range [][]string{
		nil, {""}, {"private credential"}, {strings.Repeat("private", 100)},
		{requestNonce, requestNonce}, {" " + requestNonce},
		{"00000000-0000-0000-0000-000000000000"},
		{"12345678-1234-1234-8234-123456789abc"},
		{"12345678-1234-4234-0234-123456789abc"},
		{"urn:uuid:" + requestNonce}, {"12345678123442348234123456789abc"},
	} {
		class, digest := clientRequestMarker(values)
		want := "invalid"
		if values == nil {
			want = "missing"
		}
		if class != want || digest != "missing" {
			t.Fatal("invalid metadata accepted")
		}
	}
	class, digest := clientRequestMarker([]string{requestNonce})
	want := sha256.Sum256([]byte("ornith-client-request-v1:" + requestNonce))
	if class != "uuid_v4" || digest != fmt.Sprintf("%x", want) {
		t.Fatal("nonce digest mismatch")
	}
	_, uppercase := clientRequestMarker([]string{strings.ToUpper(requestNonce)})
	if digest != uppercase {
		t.Fatal("canonical case must share a digest")
	}
}

// Use real TCP cancellation, the actual Gin middleware and production formatter.
// A reused client nonce can join requests across connections; it does not identify
// a caller or prove why that caller canceled.
func TestClientRequestMarkerRealTCPAndImmutableSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := test.NewGlobal()
	defer func() { log.StandardLogger().ReplaceHooks(previous) }()
	engine := gin.New()
	engine.Use(logging.GinLogrusLogger())
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	done := make(chan struct{}, 2)
	engine.POST("/v1/responses", func(c *gin.Context) {
		ctx, finish := h.GetContextWithCancel(nil, c, context.Background())
		c.Request.Header.Set("X-Client-Request-ID", "private-mutated-header")
		c.Writer.WriteHeaderNow()
		_, _ = c.Writer.WriteString("fixture\n")
		c.Writer.Flush()
		<-ctx.Done()
		finish(context.Canceled)
		done <- struct{}{}
	})
	server := httptest.NewUnstartedServer(engine)
	server.Config.ConnContext = logging.ConnectionContext
	server.Start()
	defer server.Close()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", nil)
		req.Header.Set("X-Client-Request-ID", requestNonce)
		req.Header.Set("Authorization", "private-auth-fixture")
		transport := &http.Transport{DisableKeepAlives: true}
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		response, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		_, err = io.ReadFull(response.Body, buf)
		cancel()
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancellation stalled")
		}
	}
	_, want := clientRequestMarker([]string{requestNonce})
	connections, requests := map[string]bool{}, map[string]bool{}
	contexts, returns := 0, 0
	for _, entry := range hook.AllEntries() {
		if !strings.HasPrefix(entry.Message, "request_cancel_trace ") {
			continue
		}
		formatted, err := (&logging.LogFormatter{}).Format(entry)
		if err != nil || !strings.Contains(string(formatted), "client_request_hash="+want) {
			t.Fatal("formatter lost digest")
		}
		for _, secret := range []string{requestNonce, "private-mutated-header", "private-auth-fixture"} {
			if strings.Contains(string(formatted), secret) || strings.Contains(fmt.Sprint(entry.Data), secret) {
				t.Fatal("raw metadata leaked")
			}
		}
		if entry.Data["client_request_marker"] != "uuid_v4" {
			t.Fatal("snapshot changed")
		}
		connections[fmt.Sprint(entry.Data["connection_id"])] = true
		requests[fmt.Sprint(entry.Data["request_id"])] = true
		if entry.Data["event"] == "request_context_done" {
			contexts++
		}
		if entry.Data["event"] == "handler_return" {
			returns++
		}
	}
	if len(connections) != 2 || len(requests) != 2 || contexts != 2 || returns != 2 {
		t.Fatalf("joins=%d/%d events=%d/%d", len(connections), len(requests), contexts, returns)
	}
}
