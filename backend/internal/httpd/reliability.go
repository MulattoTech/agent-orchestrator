package httpd

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/go-chi/chi/v5"
)

type ReliabilityDeps struct {
	VerifyAllowance func(context.Context) error
	Snapshot        func(context.Context) (any, error)
	UpdateWorkers   func(context.Context, domain.SessionAdmissionLimits) error
	UpdateInference func(context.Context, inferenceguard.Controls) error
	Cancel          func(context.Context, string) (bool, error)
}

//go:embed reliability.html
var reliabilityHTML string

// This operator surface is loopback-only even when a mobile LAN listener shares
// the router. Browser mutations require exact same origin; CLI calls omit Origin.
func reliabilityLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Del("Origin")
	if !localControlRequest(copy) {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host
}
func mountReliability(r chi.Router, d *ReliabilityDeps) {
	if d == nil {
		return
	}
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			if !reliabilityLocal(r) {
				http.Error(w, "loopback operator access required", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	r.Get("/internal/reliability/dashboard", wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		io.WriteString(w, reliabilityHTML)
	}))
	r.Get("/internal/reliability", wrap(func(w http.ResponseWriter, r *http.Request) {
		if d.Snapshot == nil {
			http.Error(w, "accounting unavailable", 503)
			return
		}
		value, err := d.Snapshot(r.Context())
		if err != nil {
			http.Error(w, "accounting observation unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(value)
	}))
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "application/json required", 415)
			return false
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			http.Error(w, "invalid control request", 400)
			return false
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "one control document required", 400)
			return false
		}
		return true
	}
	r.Put("/internal/reliability/workers", wrap(func(w http.ResponseWriter, r *http.Request) {
		if d.UpdateWorkers == nil {
			http.Error(w, "worker controls unavailable", 503)
			return
		}
		var c domain.SessionAdmissionLimits
		if !decode(w, r, &c) {
			return
		}
		if err := d.UpdateWorkers(r.Context(), c); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Put("/internal/reliability/inference", wrap(func(w http.ResponseWriter, r *http.Request) {
		if d.UpdateInference == nil {
			http.Error(w, "inference controls unavailable", 503)
			return
		}
		var c inferenceguard.Controls
		if !decode(w, r, &c) {
			return
		}
		if err := d.UpdateInference(r.Context(), c); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Post("/internal/reliability/allowance/verify", wrap(func(w http.ResponseWriter, r *http.Request) {
		if d.VerifyAllowance == nil {
			http.Error(w, "allowance verifier unavailable", 503)
			return
		}
		var request struct {
			Verify bool `json:"verify"`
		}
		if !decode(w, r, &request) {
			return
		}
		if !request.Verify {
			http.Error(w, "explicit verification action required", 400)
			return
		}
		if err := d.VerifyAllowance(r.Context()); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Post("/internal/reliability/queue/{requestID}/cancel", wrap(func(w http.ResponseWriter, r *http.Request) {
		if d.Cancel == nil {
			http.Error(w, "queue controls unavailable", 503)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "application/json required", 415)
			return
		}
		ok, err := d.Cancel(r.Context(), chi.URLParam(r, "requestID"))
		if err != nil {
			http.Error(w, "queue cancellation failed", 409)
			return
		}
		if !ok {
			http.Error(w, "request is not cancellable", 409)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
