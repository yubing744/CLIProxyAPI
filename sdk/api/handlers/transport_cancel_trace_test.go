package handlers

import (
	"context"
	"fmt"
	"io"
	"net"
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

func TestTransportConnectionCorrelatesReuseAndRealDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := test.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(old)
	observed := make(chan logging.TransportConnection, 3)
	done := make(chan struct{})
	engine := gin.New()
	engine.GET("/:mode", func(c *gin.Context) {
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), "abcdef01"))
		h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		ctx, finish := h.GetContextWithCancel(nil, c, context.Background())
		observed <- logging.GetTransportConnection(c.Request.Context())
		if c.Param("mode") == "block" {
			select {
			case <-ctx.Done():
				finish(ctx.Err())
			case <-time.After(3 * time.Second):
				finish(context.DeadlineExceeded)
			}
			close(done)
			return
		}
		c.String(200, "ok")
		finish(nil)
	})
	srv := httptest.NewUnstartedServer(engine)
	srv.Config.ConnContext = logging.ConnectionContext
	srv.Start()
	defer srv.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL + "/normal")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-observed, <-observed
	if first.ID != second.ID || first.Network != "tcp" || first.PeerClass != "loopback" {
		t.Fatalf("keepalive connection attribution: %v / %v", first, second)
	}
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = fmt.Fprint(conn, "GET /block HTTP/1.1\r\nHost: synthetic.invalid\r\nUser-Agent: Codex/private-agent\r\nAuthorization: private-credential\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var third logging.TransportConnection
	select {
	case third = <-observed:
	case <-time.After(3 * time.Second):
		t.Fatal("blocking request not accepted")
	}
	if third.ID == first.ID {
		t.Fatal("new TCP connection reused ID")
	}
	conn.(*net.TCPConn).SetLinger(0)
	conn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("real reset did not cancel handler")
	}
	matched := 0
	for _, entry := range hook.AllEntries() {
		if entry.Data["connection_id"] != third.ID {
			continue
		}
		matched++
		formatted, err := (&logging.LogFormatter{}).Format(entry)
		text := string(formatted)
		if err != nil || !strings.Contains(text, "[abcdef01]") || !strings.Contains(text, "connection_id="+third.ID) || !strings.Contains(text, "transport_network=tcp transport_peer_class=loopback") || entry.Data["reason"] != "canceled" {
			t.Fatalf("connection trace lost or wrong: %s (%v)", text, err)
		}
		for _, private := range []string{"private", "synthetic.invalid", srv.Listener.Addr().String(), "127.0.0.1"} {
			if strings.Contains(text, private) {
				t.Fatalf("private transport/request metadata leaked: %s", text)
			}
		}
	}
	if matched != 2 {
		t.Fatalf("expected request cancellation and terminal on same connection, got %d", matched)
	}
}
