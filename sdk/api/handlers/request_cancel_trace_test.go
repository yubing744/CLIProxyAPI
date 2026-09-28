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
		handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		ctx, finish := handler.GetContextWithCancel(nil, c, context.Background())
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
			if entry.Message != "request_cancel_trace" {
				continue
			}
			if strings.Contains(fmt.Sprint(entry.Data), "private") {
				t.Fatal("private contents in trace")
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
			}
		}
		if returns != 1 || (clientCanceled && clientEvents != 1) {
			t.Fatalf("returns=%d request events=%d", returns, clientEvents)
		}
	}
}
