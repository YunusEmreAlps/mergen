package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultListenAddr      = ":8080"
	defaultControlPlaneURL = "http://127.0.0.1:1323"
)

type Config struct {
	ListenAddr      string                 `json:"listen_addr"`
	ControlPlaneURL string                 `json:"control_plane_url"`
	HealthPath      string                 `json:"health_path"`
	Routes          map[string]RouteConfig `json:"routes"`
}

type RouteConfig struct {
	MachineID      string `json:"machine_id"`
	StaticEndpoint string `json:"static_endpoint"`
}

type Proxy struct {
	cfg        Config
	routes     map[string]RouteConfig
	client     *http.Client
	transport  *http.Transport
	routeMutex sync.RWMutex
	debug      bool
}

type machineStatus struct {
	ID            string `json:"id"`
	GuestAddress  string `json:"guest_address"`
	GuestHTTPPort int    `json:"guest_http_port"`
	GuestHTTPURL  string `json:"guest_http_url"`
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		ListenAddr:      defaultListenAddr,
		ControlPlaneURL: defaultControlPlaneURL,
		HealthPath:      "/healthz",
		Routes:          map[string]RouteConfig{},
	}

	if path == "" {
		path = os.Getenv("MERGEN_PROXY_CONFIG")
	}
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read proxy config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse proxy config: %w", err)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.ControlPlaneURL == "" {
		cfg.ControlPlaneURL = defaultControlPlaneURL
	}
	if cfg.Routes == nil {
		cfg.Routes = map[string]RouteConfig{}
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/healthz"
	}
	return cfg, nil
}

func New(cfg Config) (*Proxy, error) {
	if cfg.ControlPlaneURL == "" {
		cfg.ControlPlaneURL = defaultControlPlaneURL
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/healthz"
	}
	if cfg.Routes == nil {
		cfg.Routes = map[string]RouteConfig{}
	}

	transport := &http.Transport{}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	debug := false
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug", "1":
		debug = true
	}

	return &Proxy{
		cfg:       cfg,
		routes:    cfg.Routes,
		client:    client,
		transport: transport,
		debug:     debug,
	}, nil
}

func (p *Proxy) Serve(ctx context.Context) error {
	server := &http.Server{Addr: p.cfg.ListenAddr, Handler: p}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	start := time.Now()
	recorder := &responseRecorder{ResponseWriter: w}
	var target string
	var errMsg string

	if p.debug {
		log.Printf("proxy request start host=%s method=%s path=%s", host, r.Method, r.URL.Path)
		defer func() {
			duration := time.Since(start)
			status := recorder.Status()
			if errMsg != "" {
				log.Printf("proxy request done host=%s method=%s path=%s status=%d duration=%s error=%s", host, r.Method, r.URL.Path, status, duration, errMsg)
				return
			}
			log.Printf("proxy request done host=%s method=%s path=%s target=%s status=%d duration=%s", host, r.Method, r.URL.Path, target, status, duration)
		}()
	}

	if p.cfg.HealthPath != "" && r.URL.Path == p.cfg.HealthPath {
		recorder.Header().Set("Content-Type", "application/json")
		_, _ = recorder.Write([]byte(`{"status":"ok"}`))
		return
	}

	p.routeMutex.RLock()
	route, ok := p.routes[host]
	p.routeMutex.RUnlock()
	if !ok {
		errMsg = "no route for host"
		http.Error(recorder, errMsg, http.StatusBadGateway)
		return
	}

	targetURL, err := p.resolveTarget(r.Context(), route)
	if err != nil {
		errMsg = err.Error()
		http.Error(recorder, errMsg, http.StatusBadGateway)
		return
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		errMsg = fmt.Sprintf("invalid target url: %v", err)
		http.Error(recorder, errMsg, http.StatusBadGateway)
		return
	}

	target = parsed.String()

	proxy := httputil.NewSingleHostReverseProxy(parsed)
	proxy.Transport = p.transport
	proxy.ErrorHandler = func(resp http.ResponseWriter, req *http.Request, e error) {
		errMsg = fmt.Sprintf("proxy error: %v", e)
		http.Error(resp, errMsg, http.StatusBadGateway)
	}
	proxy.ServeHTTP(recorder, r)
}

func (p *Proxy) resolveTarget(ctx context.Context, route RouteConfig) (string, error) {
	if route.StaticEndpoint != "" {
		return route.StaticEndpoint, nil
	}
	if route.MachineID == "" {
		return "", errors.New("route missing machine_id")
	}

	status, err := p.fetchMachineStatus(ctx, route.MachineID)
	if err != nil {
		return "", err
	}

	if status.GuestHTTPURL != "" {
		return status.GuestHTTPURL, nil
	}

	if status.GuestAddress != "" {
		port := status.GuestHTTPPort
		if port <= 0 {
			port = 80
		}
		return fmt.Sprintf("http://%s:%d", status.GuestAddress, port), nil
	}

	return "", fmt.Errorf("machine %s has no guest endpoint", route.MachineID)
}

func (p *Proxy) fetchMachineStatus(ctx context.Context, machineID string) (*machineStatus, error) {
	base := strings.TrimSuffix(p.cfg.ControlPlaneURL, "/")
	url := fmt.Sprintf("%s/machines/%s", base, machineID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control plane request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("control plane returned %d", resp.StatusCode)
	}

	var status machineStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}

	return &status, nil
}

func hostOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if strings.HasPrefix(hostport, "[") {
		if i := strings.LastIndex(hostport, "]"); i != -1 {
			return strings.ToLower(hostport[:i+1])
		}
	}
	if i := strings.Index(hostport, ":"); i != -1 {
		hostport = hostport[:i]
	}
	return strings.ToLower(hostport)
}

func (p *Proxy) UpdateRoute(host string, route RouteConfig) {
	p.routeMutex.Lock()
	defer p.routeMutex.Unlock()
	if p.routes == nil {
		p.routes = map[string]RouteConfig{}
	}
	p.routes[strings.ToLower(host)] = route
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
