package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alperreha/mergen/internal/controlplane"
	"github.com/alperreha/mergen/internal/proxy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "control-plane":
		runControlPlane(args)
	case "proxy":
		runProxy(args)
	case "serve":
		controlPlaneServe(args)
	case "create":
		runCreate(args)
	case "stop":
		runStop(args)
	case "delete":
		runDelete(args)
	case "console":
		runConsole(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "mergenc commands:\n")
	fmt.Fprintf(os.Stderr, "  serve [--listen addr]\n")
	fmt.Fprintf(os.Stderr, "  create [--url addr] [--cpus N] [--memory MB] name\n")
	fmt.Fprintf(os.Stderr, "  stop [--url addr] name\n")
	fmt.Fprintf(os.Stderr, "  delete [--url addr] name\n")
	fmt.Fprintf(os.Stderr, "  console --id name [--url addr]\n")
	fmt.Fprintf(os.Stderr, "  proxy serve [flags]\n")
	fmt.Fprintf(os.Stderr, "  control-plane serve|console ... (legacy)\n")
}

func runControlPlane(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "missing subcommand for control-plane\n")
		usage()
		os.Exit(1)
	}
	sub := args[0]
	switch sub {
	case "serve":
		controlPlaneServe(args[1:])
	case "console":
		controlPlaneConsole(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintf(os.Stderr, "usage: mergenc control-plane serve [--listen addr]\n")
		fmt.Fprintf(os.Stderr, "       mergenc control-plane console --id machine-id [--url http://127.0.0.1:1323]\n")
	default:
		fmt.Fprintf(os.Stderr, "unknown control-plane subcommand: %s\n", sub)
		os.Exit(1)
	}
}

func runCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	baseURL := fs.String("url", "http://127.0.0.1:1323", "control plane base URL")
	cpus := fs.Int("cpus", 1, "number of virtual CPUs")
	memory := fs.Int("memory", 512, "memory size in MB")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc create [--url addr] [--cpus N] [--memory MB] name\n")
		os.Exit(1)
	}

	name := fs.Arg(0)
	payload := map[string]int64{
		"cpu_count":   int64(*cpus),
		"mem_size_mb": int64(*memory),
	}

	ctx, cancel := signalContext()
	defer cancel()

	var status controlplane.MachineStatus
	if err := performRequest(ctx, http.MethodPost, *baseURL, fmt.Sprintf("/create/%s", name), payload, &status); err != nil {
		fmt.Fprintf(os.Stderr, "create failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s status: %s\n", status.ID, status.Status)
}

func runStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	baseURL := fs.String("url", "http://127.0.0.1:1323", "control plane base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc stop [--url addr] name\n")
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	name := fs.Arg(0)
	var status controlplane.MachineStatus
	if err := performRequest(ctx, http.MethodPost, *baseURL, fmt.Sprintf("/stop/%s", name), nil, &status); err != nil {
		fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s status: %s\n", status.ID, status.Status)
}

func runDelete(args []string) {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	baseURL := fs.String("url", "http://127.0.0.1:1323", "control plane base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc delete [--url addr] name\n")
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	name := fs.Arg(0)
	if err := performRequest(ctx, http.MethodDelete, *baseURL, fmt.Sprintf("/delete/%s", name), nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "delete failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s deleted\n", name)
}

func runConsole(args []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append([]string{"--id", args[0]}, args[1:]...)
	}
	controlPlaneConsole(args)
}

func controlPlaneServe(args []string) {
	fs := flag.NewFlagSet("control-plane serve", flag.ExitOnError)
	listen := fs.String("listen", ":1323", "HTTP listen address for the control plane")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	manager, err := controlplane.NewMachineManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control plane init failed: %v\n", err)
		os.Exit(1)
	}

	server := controlplane.NewServer(manager)

	ctx, cancel := signalContext()
	defer cancel()

	if err := server.Serve(ctx, *listen); err != nil {
		fmt.Fprintf(os.Stderr, "control plane error: %v\n", err)
		os.Exit(1)
	}
}

func controlPlaneConsole(args []string) {
	fs := flag.NewFlagSet("control-plane console", flag.ExitOnError)
	machineID := fs.String("id", "", "machine identifier")
	baseURL := fs.String("url", "http://127.0.0.1:1323", "control plane base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if strings.TrimSpace(*machineID) == "" {
		fmt.Fprintf(os.Stderr, "machine id is required\n")
		os.Exit(1)
	}

	conn, err := dialConsole(*baseURL, *machineID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, cancel := signalContext()
	defer cancel()

	errCh := make(chan error, 2)

	go func() {
		_, copyErr := io.Copy(conn, os.Stdin)
		errCh <- copyErr
	}()

	go func() {
		_, copyErr := io.Copy(os.Stdout, conn)
		errCh <- copyErr
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "console error: %v\n", err)
		}
	}
}

func dialConsole(base, machineID string) (net.Conn, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}

	if parsed.Scheme == "" {
		parsed.Scheme = "http"
	}

	if parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported scheme %s", parsed.Scheme)
	}

	host := parsed.Host
	if host == "" {
		host = parsed.Path
		parsed.Path = ""
	}
	if host == "" {
		host = "127.0.0.1:1323"
	}

	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, "80")
	}

	path := strings.TrimRight(parsed.Path, "/") + "/console/" + machineID
	if path == "" {
		path = "/"
	}

	tcpConn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}

	if err := sendConsoleUpgrade(tcpConn, host, path); err != nil {
		tcpConn.Close()
		return nil, err
	}

	buffered := bufio.NewReader(tcpConn)
	if err := readUpgradeResponse(buffered); err != nil {
		tcpConn.Close()
		return nil, err
	}

	return &bufferedConn{Conn: tcpConn, reader: buffered}, nil
}

func sendConsoleUpgrade(conn net.Conn, host, path string) error {
	request := fmt.Sprintf("GET %s HTTP/1.1\r\n", path)
	if _, err := io.WriteString(conn, request); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, fmt.Sprintf("Host: %s\r\n", strings.TrimPrefix(host, "//"))); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "Upgrade: mergen-console\r\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "Connection: Upgrade\r\n\r\n"); err != nil {
		return err
	}
	return nil
}

func readUpgradeResponse(reader *bufio.Reader) error {
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(statusLine, " 101 ") {
		return fmt.Errorf("unexpected response: %s", strings.TrimSpace(statusLine))
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			break
		}
	}
	return nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func runProxy(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "missing subcommand for proxy\n")
		usage()
		os.Exit(1)
	}
	sub := args[0]
	switch sub {
	case "serve":
		proxyServe(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintf(os.Stderr, "usage: mergenc proxy serve [--config path] [--listen addr] [--control-plane-url url]\n")
	default:
		fmt.Fprintf(os.Stderr, "unknown proxy subcommand: %s\n", sub)
		os.Exit(1)
	}
}

func proxyServe(args []string) {
	fs := flag.NewFlagSet("proxy serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to JSON proxy configuration")
	listenOverride := fs.String("listen", "", "override listen address")
	cpOverride := fs.String("control-plane-url", "", "override control plane base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	cfg, err := proxy.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load proxy config failed: %v\n", err)
		os.Exit(1)
	}

	if *listenOverride != "" {
		cfg.ListenAddr = *listenOverride
	}
	if *cpOverride != "" {
		cfg.ControlPlaneURL = strings.TrimRight(*cpOverride, "/")
	}

	proxyServer, err := proxy.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy init failed: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := proxyServer.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "proxy error: %v\n", err)
		os.Exit(1)
	}
}

func performRequest(ctx context.Context, method, baseURL, path string, payload any, out any) error {
	trimmed := strings.TrimRight(baseURL, "/")
	if trimmed == "" {
		trimmed = "http://127.0.0.1:1323"
	}
	fullURL := trimmed + path

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if len(data) == 0 {
			return fmt.Errorf("request failed with status %d", resp.StatusCode)
		}
		return fmt.Errorf("request failed: %s", strings.TrimSpace(string(data)))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		time.Sleep(100 * time.Millisecond)
		signal.Stop(sigCh)
		close(sigCh)
	}()

	return ctx, cancel
}
