package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	return &Proxy{
		cfg:       cfg,
		routes:    cfg.Routes,
		client:    client,
		transport: transport,
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
	if p.cfg.HealthPath != "" && r.URL.Path == p.cfg.HealthPath {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}

	p.routeMutex.RLock()
	route, ok := p.routes[host]
	p.routeMutex.RUnlock()
	if !ok {
		http.Error(w, "no route for host", http.StatusBadGateway)
		return
	}

	targetURL, err := p.resolveTarget(r.Context(), route)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid target url: %v", err), http.StatusBadGateway)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(parsed)
	proxy.Transport = p.transport
	proxy.ErrorHandler = func(resp http.ResponseWriter, req *http.Request, e error) {
		http.Error(resp, fmt.Sprintf("proxy error: %v", e), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
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
