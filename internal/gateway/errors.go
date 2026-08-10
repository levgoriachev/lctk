// Package gateway serves the project-scoped MCP endpoint.
//
// The route is the only authority on scope, per [ADR-0001]. The gateway takes
// project_id from the path, validates OAuth audience against that project, and
// resolves the canonical root from the registry. A project identifier supplied in
// a tool argument is never consulted.
//
// [ADR-0001]: ../../docs/adr/0001-route-bound-project-scope.md
package gateway

import (
	"encoding/json"
	"net/http"
)

// Typed error codes from docs/project-lifecycle.md. The consumer of this
// endpoint is a coding agent, so an error has to say what happened, whether
// waiting helps, and what to do next.
const (
	CodeProjectNotFound     = "PROJECT_NOT_FOUND"
	CodeProjectStopped      = "PROJECT_STOPPED"
	CodeProjectStarting     = "PROJECT_STARTING"
	CodeServiceUnavailable  = "SERVICE_UNAVAILABLE"
	CodeAuthRequired        = "AUTH_REQUIRED"
	CodeAuthForbidden       = "AUTH_FORBIDDEN"
	CodeInternalError       = "INTERNAL_ERROR"
	CodeMethodNotAllowed    = "METHOD_NOT_ALLOWED"
	CodeProjectPathMissing  = "PROJECT_PATH_MISSING"
	CodeRuntimeUnavailable  = "RUNTIME_UNAVAILABLE"
	CodeRuntimeUnsuitable   = "RUNTIME_UNSUITABLE"
	CodeRegistryUnavailable = "REGISTRY_UNAVAILABLE"
)

// TypedError is the error envelope every failure uses.
type TypedError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RecommendedAction string `json:"recommended_action,omitempty"`
	ProjectID         string `json:"project_id,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error TypedError `json:"error"`
}

// writeError emits a typed failure.
//
// The body is written even for an authentication failure, because a caller that
// cannot tell "wrong credential" from "wrong project" cannot correct itself.
func writeError(w http.ResponseWriter, status int, e TypedError) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lctk"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: e})
}
