package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

type cancellationOriginKey struct{}
type cancellationOrigin struct {
	clientFamily, peerClass, requestDeadline, parentDeadline string
	transport                                                logging.TransportConnection
	clientRequestMarker, clientRequestHash                   string
}

// An opt-in request nonce, never an authenticated identity. Accept only one
// canonical random UUID so arbitrary headers, session IDs and secrets cannot
// become log contents. Keep a domain-separated digest, not the original value.
func clientRequestMarker(values []string) (string, string) {
	if len(values) == 0 {
		return "missing", "missing"
	}
	if len(values) != 1 || len(values[0]) != 36 {
		return "invalid", "missing"
	}
	id, err := uuid.Parse(values[0])
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != strings.ToLower(values[0]) {
		return "invalid", "missing"
	}
	digest := sha256.Sum256([]byte("ornith-client-request-v1:" + id.String()))
	return "uuid_v4", fmt.Sprintf("%x", digest)
}

// These are unauthenticated hints, not caller identities. Never log raw UA,
// addresses, forwarded headers, credentials, or arbitrary deadline values.
func clientFamily(value string) string {
	if strings.TrimSpace(value) == "" {
		return "missing"
	}
	if len(value) > 256 {
		value = value[:256]
	}
	value = strings.ToLower(value)
	for _, candidate := range []struct{ needle, class string }{
		{"fractalbot", "fractalbot"}, {"codex", "codex"},
		{"openai-python", "openai_python"}, {"openai/node", "openai_node"},
		{"python-urllib", "python_urllib"}, {"python-requests", "python_requests"},
		{"go-http-client", "go_http"}, {"curl/", "curl"},
	} {
		if strings.Contains(value, candidate.needle) {
			return candidate.class
		}
	}
	return "other"
}

func peerClass(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "missing"
	}
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "non_ip"
	}
	// ParseAddr accepts IPv6 zones from net/http RemoteAddr. Unmap keeps
	// IPv4-mapped loopback/private peers equivalent to their IPv4 form.
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return "private"
	}
	return "other_ip"
}

func deadlineClass(ctx context.Context, at time.Time) string {
	if ctx == nil {
		return "none"
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return "none"
	}
	remaining := deadline.Sub(at)
	switch {
	case remaining <= 0:
		return "expired"
	case remaining <= 5*time.Second:
		return "le_5s"
	case remaining <= 30*time.Second:
		return "le_30s"
	case remaining <= 120*time.Second:
		return "le_120s"
	default:
		return "gt_120s"
	}
}

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

// Record bounded IDs, error classes and origin enums; never emit raw metadata.
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
	// The production formatter filters structured fields. Render these bounded
	// values explicitly so the diagnostic survives the real log output path.
	message := fmt.Sprintf("request_cancel_trace event=%s reason=%s elapsed_ms=%d", event, fields["reason"], fields["elapsed_ms"])
	if origin, ok := ctx.Value(cancellationOriginKey{}).(cancellationOrigin); ok {
		fields["client_family"] = origin.clientFamily
		fields["peer_class"] = origin.peerClass
		fields["request_deadline"] = origin.requestDeadline
		fields["parent_deadline"] = origin.parentDeadline
		message += fmt.Sprintf(" client_family=%s peer_class=%s request_deadline=%s parent_deadline=%s",
			origin.clientFamily, origin.peerClass, origin.requestDeadline, origin.parentDeadline)
		fields["connection_id"] = origin.transport.ID
		fields["transport_network"] = origin.transport.Network
		fields["transport_peer_class"] = origin.transport.PeerClass
		message += fmt.Sprintf(" connection_id=%s transport_network=%s transport_peer_class=%s",
			origin.transport.ID, origin.transport.Network, origin.transport.PeerClass)
		fields["client_request_marker"] = origin.clientRequestMarker
		fields["client_request_hash"] = origin.clientRequestHash
		message += fmt.Sprintf(" client_request_marker=%s client_request_hash=%s",
			origin.clientRequestMarker, origin.clientRequestHash)
	}
	if c != nil && c.Request != nil {
		fields["request_context"] = cancellationClass(c.Request.Context().Err())
		fields["response_status"] = c.Writer.Status()
		fields["headers_committed"] = c.Writer.Written()
		message += fmt.Sprintf(" request_context=%s response_status=%d headers_committed=%t", fields["request_context"], fields["response_status"], fields["headers_committed"])
	}
	log.WithFields(fields).Info(message)
}
