package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alperreha/mergen/internal/vm"
)

const (
	defaultListenAddr   = ":1323"
	defaultStopTimeout  = 30 * time.Second
	shutdownGracePeriod = 5 * time.Second
	headerContentType   = "Content-Type"
	contentTypeJSON     = "application/json"
)

// Server exposes the Firecracker control plane over HTTP.
type Server struct {
	runtime vm.Runtime
	mux     *http.ServeMux
}

// NewServer constructs a Server instance using the provided runtime.
func NewServer(runtime vm.Runtime) *Server {
	srv := &Server{runtime: runtime}
	srv.mux = srv.routes()
	return srv
}

// Serve starts the HTTP server and blocks until the supplied context is cancelled
// or the listener encounters an error.
func (s *Server) Serve(ctx context.Context, listenAddr string) error {
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	server := &http.Server{ //nolint:gosec // listen address is user-supplied
		Addr:    listenAddr,
		Handler: s.mux,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/create/", s.handleCreate)
	mux.HandleFunc("/stop/", s.handleStop)
	mux.HandleFunc("/delete/", s.handleDelete)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	name, ok := extractName(r.URL.Path, "/create/")
	if !ok {
		notFound(w)
		return
	}

	var req createRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		badRequest(w, err.Error())
		return
	}

	opts := vm.CreateOptions{
		ID:   name,
		Spec: vm.Spec{CPUCount: req.CPUCount, MemSizeMb: req.MemSizeMb},
	}
	if req.TapDevice != "" {
		opts.Network = &vm.NetworkOptions{
			TapDevice:  req.TapDevice,
			HostIPCIDR: req.HostIPCIDR,
			MacAddress: req.MacAddress,
		}
	}

	status, err := vm.Create(r.Context(), s.runtime, opts)
	if err != nil {
		writeOperationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	name, ok := extractName(r.URL.Path, "/stop/")
	if !ok {
		notFound(w)
		return
	}

	var req stopRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		badRequest(w, err.Error())
		return
	}

	timeout := defaultStopTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	status, err := vm.Stop(ctx, s.runtime, name)
	if err != nil {
		writeOperationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}

	name, ok := extractName(r.URL.Path, "/delete/")
	if !ok {
		notFound(w)
		return
	}

	removeTap := true
	if raw := r.URL.Query().Get("remove_tap"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			badRequest(w, "invalid remove_tap value")
			return
		}
		removeTap = parsed
	}

	if err := vm.Delete(r.Context(), s.runtime, name, removeTap); err != nil {
		writeOperationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": name, "deleted": true})
}

type createRequest struct {
	CPUCount   int64  `json:"cpu_count"`
	MemSizeMb  int64  `json:"mem_size_mb"`
	TapDevice  string `json:"tap_device"`
	HostIPCIDR string `json:"host_ip_cidr"`
	MacAddress string `json:"mac_address"`
}

type stopRequest struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

func decodeJSON(body io.ReadCloser, dst any) error {
	if body == nil {
		return nil
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func notFound(w http.ResponseWriter) {
	http.Error(w, "not found", http.StatusNotFound)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": message})
}

func writeOperationError(w http.ResponseWriter, err error) {
	if errors.Is(err, vm.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func extractName(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(path, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
