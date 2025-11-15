package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultBridgeName  = "docker0"
	rootImageName      = "rootfs.ext4"
	kernelImageName    = "vmlinux.bin"
	logFileName        = "firecracker.log"
	sockFileName       = "firecracker.sock"
	configFileName     = "config.json"
	pidFileName        = "firecracker.pid"
	bootArgsBase       = "ro console=ttyS0 noapic reboot=k panic=1 pci=off nomodules random.trust_cpu=on"
	unixScheme         = "http://unix"
	defaultHTTPTimeout = 10 * time.Second
	defaultSocketWait  = 12 * time.Second
)

// Manager wires together image templates, per-VM volumes and Firecracker processes.
type Manager struct {
	rootDir         string
	imagesDir       string
	volumesDir      string
	firecrackerPath string
}

// ManagerConfig exposes all configurable manager knobs.
type ManagerConfig struct {
	RootDir         string
	ImagesDir       string
	VolumesDir      string
	FirecrackerPath string
}

// CreateOptions defines the metadata required to scaffold a new microVM workload.
type CreateOptions struct {
	Name      string
	CPUCount  int
	MemoryMB  int
	GuestIP   string
	GatewayIP string
	Netmask   string
	Bridge    string
	TapDevice string
	MacAddr   string
}

// Config captures the persisted metadata for a microVM instance.
type Config struct {
	Name       string        `json:"name"`
	KernelArgs string        `json:"kernel_args"`
	Paths      ConfigPaths   `json:"paths"`
	Network    NetworkConfig `json:"network"`
	Machine    MachineSpec   `json:"machine"`
	CreatedAt  time.Time     `json:"created_at"`
}

// ConfigPaths lists on-disk artefacts tied to a VM instance.
type ConfigPaths struct {
	RootFS  string `json:"rootfs"`
	Kernel  string `json:"kernel"`
	Socket  string `json:"socket"`
	LogFile string `json:"log_file"`
	Config  string `json:"config"`
	PIDFile string `json:"pid_file"`
	WorkDir string `json:"workdir"`
}

// NetworkConfig reflects bridge and tap layout.
type NetworkConfig struct {
	Bridge    string `json:"bridge"`
	TapDevice string `json:"tap_device"`
	GuestIP   string `json:"guest_ip"`
	GatewayIP string `json:"gateway_ip"`
	Netmask   string `json:"netmask"`
	MacAddr   string `json:"mac_addr"`
}

// MachineSpec stores CPU and memory sizing.
type MachineSpec struct {
	CPUCount int `json:"cpu_count"`
	MemoryMB int `json:"memory_mb"`
}

// NewManager resolves a Manager with sane defaults.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	root := cfg.RootDir
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working directory: %w", err)
		}
		root = cwd
	}

	images := cfg.ImagesDir
	if images == "" {
		images = filepath.Join(root, "images")
	} else if !filepath.IsAbs(images) {
		images = filepath.Join(root, images)
	}

	volumes := cfg.VolumesDir
	if volumes == "" {
		volumes = filepath.Join(root, "volumes")
	} else if !filepath.IsAbs(volumes) {
		volumes = filepath.Join(root, volumes)
	}

	fcPath := cfg.FirecrackerPath
	if fcPath == "" {
		fcPath = "firecracker"
	}

	return &Manager{
		rootDir:         root,
		imagesDir:       images,
		volumesDir:      volumes,
		firecrackerPath: fcPath,
	}, nil
}

// Create scaffolds a microVM directory, network layout and config artefacts.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Config, error) {
	if opts.Name == "" {
		return nil, errors.New("missing VM name")
	}
	if opts.CPUCount <= 0 {
		opts.CPUCount = 1
	}
	if opts.MemoryMB <= 0 {
		opts.MemoryMB = 512
	}
	if opts.Bridge == "" {
		opts.Bridge = defaultBridgeName
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	workDir := filepath.Join(m.volumesDir, opts.Name)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create volume directory: %w", err)
	}

	sourceRoot := filepath.Join(m.imagesDir, rootImageName)
	sourceKernel := filepath.Join(m.imagesDir, kernelImageName)

	if _, err := os.Stat(sourceRoot); err != nil {
		return nil, fmt.Errorf("template rootfs missing: %w", err)
	}
	if _, err := os.Stat(sourceKernel); err != nil {
		return nil, fmt.Errorf("template kernel missing: %w", err)
	}

	tapName := opts.TapDevice
	if tapName == "" {
		tapName = fmt.Sprintf("fc-%s-tap0", opts.Name)
	}

	mac := opts.MacAddr
	if mac == "" {
		var err error
		mac, err = generateMAC()
		if err != nil {
			return nil, fmt.Errorf("generate mac: %w", err)
		}
	}

	netmask := opts.Netmask
	if netmask == "" {
		derivedMask, err := lookupBridgeNetmask(opts.Bridge)
		if err != nil {
			return nil, fmt.Errorf("derive bridge netmask: %w", err)
		}
		netmask = derivedMask
	}

	gateway := opts.GatewayIP
	if gateway == "" {
		bridgeGateway, err := lookupBridgeGateway(opts.Bridge)
		if err != nil {
			return nil, fmt.Errorf("derive bridge gateway: %w", err)
		}
		gateway = bridgeGateway
	}

	if opts.GuestIP == "" {
		return nil, errors.New("guest IP address must be specified")
	}

	rootDst := filepath.Join(workDir, rootImageName)
	kernelDst := filepath.Join(workDir, kernelImageName)
	socketPath := filepath.Join(workDir, sockFileName)
	logPath := filepath.Join(workDir, logFileName)
	configPath := filepath.Join(workDir, configFileName)
	pidPath := filepath.Join(workDir, pidFileName)

	if err := copyFile(sourceRoot, rootDst); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}
	if err := copyFile(sourceKernel, kernelDst); err != nil {
		return nil, fmt.Errorf("copy kernel: %w", err)
	}

	if err := createLogFile(logPath); err != nil {
		return nil, fmt.Errorf("prepare log file: %w", err)
	}

	if err := prepareNetwork(tapName, opts.Bridge); err != nil {
		return nil, fmt.Errorf("configure network: %w", err)
	}

	bootArgs := fmt.Sprintf("%s ip=%s::%s:%s::eth0:off", bootArgsBase, opts.GuestIP, gateway, netmask)

	result := &Config{
		Name:       opts.Name,
		KernelArgs: bootArgs,
		Paths: ConfigPaths{
			RootFS:  rootDst,
			Kernel:  kernelDst,
			Socket:  socketPath,
			LogFile: logPath,
			Config:  configPath,
			PIDFile: pidPath,
			WorkDir: workDir,
		},
		Network: NetworkConfig{
			Bridge:    opts.Bridge,
			TapDevice: tapName,
			GuestIP:   opts.GuestIP,
			GatewayIP: gateway,
			Netmask:   netmask,
			MacAddr:   strings.ToLower(mac),
		},
		Machine: MachineSpec{
			CPUCount: opts.CPUCount,
			MemoryMB: opts.MemoryMB,
		},
		CreatedAt: time.Now().UTC(),
	}

	if err := persistConfig(result); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	// ensure stale socket/pid artefacts are absent post-create
	_ = os.Remove(socketPath)
	_ = os.Remove(pidPath)

	return result, nil
}

// Start boots an existing microVM by launching firecracker and replaying the stored config.
func (m *Manager) Start(ctx context.Context, name string) (err error) {
	cfg, err := m.LoadConfig(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(cfg.Paths.Socket); err == nil {
		return fmt.Errorf("socket already present at %s; is the VM running?", cfg.Paths.Socket)
	}

	cmd := exec.Command(m.firecrackerPath,
		"--api-sock", cfg.Paths.Socket,
		"--log-path", cfg.Paths.LogFile,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = os.Remove(cfg.Paths.Socket)
		_ = os.Remove(cfg.Paths.PIDFile)
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	if err = os.WriteFile(cfg.Paths.PIDFile, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		return fmt.Errorf("persist pid: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		_ = os.Remove(cfg.Paths.Socket)
	}()

	if err = waitForSocket(ctx, cfg.Paths.Socket); err != nil {
		return fmt.Errorf("await socket: %w", err)
	}

	httpClient := newUnixHTTPClient(cfg.Paths.Socket)

	if err = applyMachineConfig(ctx, httpClient, cfg); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}
	if err = applyBootSource(ctx, httpClient, cfg); err != nil {
		return fmt.Errorf("configure boot: %w", err)
	}
	if err = applyDrive(ctx, httpClient, cfg); err != nil {
		return fmt.Errorf("configure drive: %w", err)
	}
	if err = applyNetwork(ctx, httpClient, cfg); err != nil {
		return fmt.Errorf("configure network: %w", err)
	}
	if err = issueAction(ctx, httpClient, "InstanceStart"); err != nil {
		return fmt.Errorf("start instance: %w", err)
	}

	return nil
}

// Stop asks the running microVM to shutdown using a Ctrl+Alt+Del signal.
func (m *Manager) Stop(ctx context.Context, name string) error {
	cfg, err := m.LoadConfig(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(cfg.Paths.Socket); os.IsNotExist(err) {
		_ = os.Remove(cfg.Paths.PIDFile)
		return nil
	}

	httpClient := newUnixHTTPClient(cfg.Paths.Socket)
	if err := issueAction(ctx, httpClient, "SendCtrlAltDel"); err != nil {
		if _, statErr := os.Stat(cfg.Paths.Socket); os.IsNotExist(statErr) {
			_ = os.Remove(cfg.Paths.PIDFile)
			return nil
		}
		return err
	}

	// wait for socket removal which signals VM exit
	deadline := time.Now().Add(defaultSocketWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.Paths.Socket); os.IsNotExist(err) {
			_ = os.Remove(cfg.Paths.PIDFile)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return errors.New("timed out waiting for VM shutdown")
}

// Delete tears down the VM, removes tap devices and deletes stored artefacts.
func (m *Manager) Delete(ctx context.Context, name string) error {
	cfg, err := m.LoadConfig(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(cfg.Paths.Socket); err == nil {
		if stopErr := m.Stop(ctx, name); stopErr != nil {
			return fmt.Errorf("stop before delete: %w", stopErr)
		}
	}

	if err := removeTap(cfg.Network.TapDevice); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove tap: %w", err)
	}

	if err := os.RemoveAll(cfg.Paths.WorkDir); err != nil {
		return fmt.Errorf("remove volume: %w", err)
	}

	return nil
}

// LoadConfig reads an existing VM config from disk.
func (m *Manager) LoadConfig(name string) (*Config, error) {
	if name == "" {
		return nil, errors.New("missing VM name")
	}

	path := filepath.Join(m.volumesDir, name, configFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	return &cfg, nil
}

func persistConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.Paths.Config, data, 0o644)
}

func copyFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return nil
}

func createLogFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func prepareNetwork(tapName, bridgeName string) error {
	if err := removeTap(tapName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if _, err := net.InterfaceByName(bridgeName); err != nil {
		return fmt.Errorf("bridge %s not found: %w", bridgeName, err)
	}

	if err := runCommand("ip", "tuntap", "add", "dev", tapName, "mode", "tap"); err != nil {
		return fmt.Errorf("add tap: %w", err)
	}

	if err := runCommand("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.proxy_arp=1", tapName)); err != nil {
		return fmt.Errorf("enable proxy_arp: %w", err)
	}
	if err := runCommand("sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", tapName)); err != nil {
		return fmt.Errorf("disable ipv6: %w", err)
	}

	if err := runCommand("ip", "link", "set", "dev", tapName, "master", bridgeName); err != nil {
		return fmt.Errorf("attach tap to bridge: %w", err)
	}

	if err := runCommand("ip", "link", "set", "dev", tapName, "up"); err != nil {
		return fmt.Errorf("bring tap up: %w", err)
	}

	return nil
}

func removeTap(tapName string) error {
	if err := runCommand("ip", "link", "del", tapName); err != nil {
		if strings.Contains(err.Error(), "Cannot find device") || strings.Contains(err.Error(), "not found") {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func generateMAC() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[0] = (buf[0] | 2) & 0xfe
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]), nil
}

func lookupBridgeNetmask(bridgeName string) (string, error) {
	ipnet, err := bridgeIPv4Net(bridgeName)
	if err != nil {
		return "", err
	}
	return maskToString(ipnet.Mask)
}

func lookupBridgeGateway(bridgeName string) (string, error) {
	ipnet, err := bridgeIPv4Net(bridgeName)
	if err != nil {
		return "", err
	}
	return ipnet.IP.String(), nil
}

func bridgeIPv4Net(name string) (*net.IPNet, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet, nil
		}
	}
	return nil, fmt.Errorf("bridge %s missing IPv4 configuration", name)
}

func maskToString(mask net.IPMask) (string, error) {
	if len(mask) != 4 {
		return "", fmt.Errorf("unexpected mask length %d", len(mask))
	}
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3]), nil
}

func waitForSocket(ctx context.Context, path string) error {
	deadline := time.Now().Add(defaultSocketWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("socket %s not created within timeout", path)
}

func newUnixHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transport, Timeout: defaultHTTPTimeout}
}

func applyMachineConfig(ctx context.Context, client *http.Client, cfg *Config) error {
	payload := map[string]interface{}{
		"vcpu_count":   cfg.Machine.CPUCount,
		"mem_size_mib": cfg.Machine.MemoryMB,
	}
	return putJSON(ctx, client, "/machine-config", payload)
}

func applyBootSource(ctx context.Context, client *http.Client, cfg *Config) error {
	payload := map[string]interface{}{
		"kernel_image_path": cfg.Paths.Kernel,
		"boot_args":         cfg.KernelArgs,
	}
	return putJSON(ctx, client, "/boot-source", payload)
}

func applyDrive(ctx context.Context, client *http.Client, cfg *Config) error {
	payload := map[string]interface{}{
		"drive_id":       "rootfs",
		"path_on_host":   cfg.Paths.RootFS,
		"is_root_device": true,
		"is_read_only":   false,
	}
	return putJSON(ctx, client, "/drives/rootfs", payload)
}

func applyNetwork(ctx context.Context, client *http.Client, cfg *Config) error {
	payload := map[string]interface{}{
		"iface_id":        "eth0",
		"host_dev_name":   cfg.Network.TapDevice,
		"guest_mac":       cfg.Network.MacAddr,
		"rx_rate_limiter": nil,
		"tx_rate_limiter": nil,
	}
	return putJSON(ctx, client, "/network-interfaces/eth0", payload)
}

func issueAction(ctx context.Context, client *http.Client, action string) error {
	payload := map[string]string{"action_type": action}
	return putJSON(ctx, client, "/actions", payload)
}

func putJSON(ctx context.Context, client *http.Client, path string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, unixScheme+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker API %s failed: %s", path, strings.TrimSpace(string(b)))
	}

	return nil
}
