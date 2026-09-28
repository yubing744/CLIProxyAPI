package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

func cancellationClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "other_error"
	}
}

// Record only bounded IDs and error classes; never emit error text or caller metadata.
// Gin writer state is accessed only by the handler's return callback, not its watcher.
func logRequestCancellation(ctx context.Context, event string, err error, started time.Time, c *gin.Context) {
	id := logging.GetRequestID(ctx)
	if len(id) != 8 || strings.Trim(id, "0123456789abcdef") != "" {
		return
	}
	fields := log.Fields{
		"request_id": id,
		"event":      event,
		"reason":     cancellationClass(err),
		"elapsed_ms": time.Since(started).Milliseconds(),
	}
	if c != nil && c.Request != nil {
		fields["request_context"] = cancellationClass(c.Request.Context().Err())
		fields["response_status"] = c.Writer.Status()
		fields["headers_committed"] = c.Writer.Written()
	}
	log.WithFields(fields).Info("request_cancel_trace")
}
