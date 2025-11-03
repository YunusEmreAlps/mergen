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
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

type Spec struct {
	CPUCount  int64
	MemSizeMb int64
}

type NetworkOptions struct {
	TapDevice  string
	HostIPCIDR string
	MacAddress string
}

type CreateOptions struct {
	ID      string
	Spec    Spec
	Network *NetworkOptions
}

func Create(ctx context.Context, runtime Runtime, opts CreateOptions) (*Status, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return nil, errors.New("id is required")
	}
	spec := opts.Spec
	if spec.CPUCount <= 0 {
		spec.CPUCount = 1
	}
	if spec.MemSizeMb <= 0 {
		spec.MemSizeMb = 512
	}

	paths := runtime.MachinePaths(opts.ID)

	if _, err := os.Stat(paths.VolumeDir); err == nil {
		return nil, fmt.Errorf("machine %s already exists", opts.ID)
	}
	if err := os.MkdirAll(paths.VolumeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create volume dir: %w", err)
	}

	cleanupVolume := true
	defer func() {
		if cleanupVolume {
			_ = os.RemoveAll(paths.VolumeDir)
		}
	}()

	if err := copyFile(runtime.KernelImagePath, paths.KernelImage); err != nil {
		return nil, fmt.Errorf("copy kernel image: %w", err)
	}
	if err := copyFile(runtime.RootfsImagePath, paths.RootfsImage); err != nil {
		return nil, fmt.Errorf("copy rootfs image: %w", err)
	}

	_ = os.Remove(paths.SocketPath)
	_ = os.Remove(paths.LogFifo)
	_ = os.Remove(paths.MetricsFifo)

	if err := makeFifo(paths.LogFifo); err != nil {
		return nil, fmt.Errorf("create log fifo: %w", err)
	}
	if err := makeFifo(paths.MetricsFifo); err != nil {
		return nil, fmt.Errorf("create metrics fifo: %w", err)
	}

	logFile, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	cleanupLog := true
	defer func() {
		if cleanupLog {
			_ = logFile.Close()
			_ = os.Remove(paths.LogFile)
		}
	}()

	logger := logrus.New()
	logger.SetOutput(logFile)
	logger.SetLevel(logrus.InfoLevel)

	cfg := firecracker.Config{
		SocketPath:      paths.SocketPath,
		LogFifo:         paths.LogFifo,
		MetricsFifo:     paths.MetricsFifo,
		KernelImagePath: paths.KernelImage,
		KernelArgs:      runtime.BootArgs,
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String(fmt.Sprintf("rootfs_%s", paths.SanitizedID)),
				PathOnHost:   firecracker.String(paths.RootfsImage),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       firecracker.Int64(spec.CPUCount),
			MemSizeMib:      firecracker.Int64(spec.MemSizeMb),
			Smt:             firecracker.Bool(false),
			// TrackDirtyPages: firecracker.Bool(false),
		},
		VMID: paths.SanitizedID,
	}

	status := &Status{
		ID:          opts.ID,
		State:       "starting",
		SocketPath:  paths.SocketPath,
		VolumePath:  paths.VolumeDir,
		LogFile:     paths.LogFile,
		LogFifo:     paths.LogFifo,
		MetricsFifo: paths.MetricsFifo,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if opts.Network != nil && opts.Network.TapDevice != "" {
		mac := opts.Network.MacAddress
		if strings.TrimSpace(mac) == "" {
			generated, err := generateMAC()
			if err != nil {
				return nil, fmt.Errorf("generate mac address: %w", err)
			}
			mac = generated
		}
		status.TapDevice = opts.Network.TapDevice
		status.MacAddress = mac
		status.HostIPCIDR = opts.Network.HostIPCIDR

		if err := ensureTapDevice(ctx, opts.Network.TapDevice, opts.Network.HostIPCIDR); err != nil {
			return nil, err
		}

		cfg.NetworkInterfaces = []firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					HostDevName: opts.Network.TapDevice,
					MacAddress:  mac,
				},
			},
		}
	}

	// Use firecracker binary when making machine
	// TODO: update below cmd binary location because i updated it manually before.
	binPath := os.Getenv("MERGEN_FIRECRACKER_BIN")
	if binPath == "" {
		binPath = filepath.Join("/usr/local/bin", "firecracker")
	}
	cmd := firecracker.VMCommandBuilder{}.
		WithSocketPath(paths.SocketPath).
		WithBin(binPath).
		Build(ctx)

	machine, err := firecracker.NewMachine(ctx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return nil, fmt.Errorf("init machine: %w", err)
	}

	if err := machine.Start(ctx); err != nil {
		return nil, fmt.Errorf("start machine: %w", err)
	}

	go streamFIFO(paths.LogFifo, paths.LogFile)
	go streamFIFO(paths.MetricsFifo, "")

	status.State = "running"
	status.UpdatedAt = time.Now()
	if err := status.Save(paths.StatusFile); err != nil {
		_ = machine.Shutdown(context.Background())
		return nil, err
	}

	cleanupLog = false
	cleanupVolume = false

	go monitorMachine(machine, paths.StatusFile, logFile)

	return status, nil
}

func Stop(ctx context.Context, runtime Runtime, id string) (*Status, error) {
	paths := runtime.MachinePaths(id)
	status, err := LoadStatus(paths.StatusFile)
	if err != nil {
		return nil, err
	}

	client := newFirecrackerClient(paths.SocketPath)
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.instanceAction(stopCtx, actionRequest{ActionType: "InstanceStop"}); err != nil {
		if !isNotExist(err) {
			return nil, err
		}
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWait()
	_ = waitForSocketRemoval(waitCtx, paths.SocketPath)

	status.State = "stopped"
	status.UpdatedAt = time.Now()
	status.Error = ""
	if err := status.Save(paths.StatusFile); err != nil {
		return nil, err
	}
	return status, nil
}

func Delete(ctx context.Context, runtime Runtime, id string, removeTap bool) error {
	paths := runtime.MachinePaths(id)
	status, err := LoadStatus(paths.StatusFile)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	if _, stopErr := Stop(ctx, runtime, id); stopErr != nil && !errors.Is(stopErr, ErrNotFound) {
		return stopErr
	}

	_ = os.Remove(paths.SocketPath)
	_ = os.Remove(paths.LogFifo)
	_ = os.Remove(paths.MetricsFifo)
	if err := os.RemoveAll(paths.VolumeDir); err != nil {
		return fmt.Errorf("remove volume dir: %w", err)
	}

	if removeTap && status != nil && status.TapDevice != "" {
		_ = runCommand(ctx, "ip", "link", "del", status.TapDevice)
	}

	return nil
}

func monitorMachine(machine *firecracker.Machine, statusPath string, logFile *os.File) {
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	err := machine.Wait(context.Background())

	st, loadErr := LoadStatus(statusPath)
	if loadErr != nil {
		return
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		st.State = "error"
		st.Error = err.Error()
	} else {
		st.State = "stopped"
		st.Error = ""
	}
	st.UpdatedAt = time.Now()
	_ = st.Save(statusPath)
}

func ensureTapDevice(ctx context.Context, tapName, hostCIDR string) error {
	if tapName == "" {
		return nil
	}

	if err := runCommand(ctx, "ip", "link", "show", tapName); err != nil {
		if err := runCommand(ctx, "ip", "tuntap", "add", "dev", tapName, "mode", "tap"); err != nil {
			return fmt.Errorf("create tap device %s: %w", tapName, err)
		}
	}

	if hostCIDR != "" {
		if err := runCommandAllowExists(ctx, "ip", "addr", "add", hostCIDR, "dev", tapName); err != nil {
			return fmt.Errorf("assign ip to tap %s: %w", tapName, err)
		}
	}

	if err := runCommand(ctx, "ip", "link", "set", tapName, "up"); err != nil {
		return fmt.Errorf("bring tap %s up: %w", tapName, err)
	}
	return nil
}

func generateMAC() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[0] &= 0xfe
	buf[0] |= 0x02
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]), nil
}

func makeFifo(path string) error {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

func streamFIFO(path, logPath string) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()

	var writer io.Writer
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			writer = io.Discard
		} else {
			defer f.Close()
			writer = f
		}
	} else {
		writer = io.Discard
	}

	_, _ = io.Copy(writer, file)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

type actionRequest struct {
	ActionType string `json:"action_type"`
}

type firecrackerClient struct {
	httpClient *http.Client
}

func newFirecrackerClient(socketPath string) *firecrackerClient {
	transport := &http.Transport{}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{}
		return d.DialContext(ctx, "unix", socketPath)
	}
	transport.DisableCompression = true
	return &firecrackerClient{httpClient: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
}

func (c *firecrackerClient) instanceAction(ctx context.Context, payload actionRequest) error {
	return c.do(ctx, http.MethodPut, "/actions", payload)
}

func (c *firecrackerClient) do(ctx context.Context, method, path string, payload any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("firecracker %s %s failed: %s", method, path, strings.TrimSpace(string(data)))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(stderr.String())
		if output != "" {
			return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), output)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func runCommandAllowExists(ctx context.Context, name string, args ...string) error {
	err := runCommand(ctx, name, args...)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "exists") {
		return nil
	}
	return err
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such file") {
		return true
	}
	return false
}

func waitForSocketRemoval(ctx context.Context, socketPath string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(socketPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func SanitizeID(id string) string {
	if id == "" {
		return ""
	}
	buf := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			buf = append(buf, r)
		case r >= 'A' && r <= 'Z':
			buf = append(buf, r)
		case r >= '0' && r <= '9':
			buf = append(buf, r)
		case r == '_':
			buf = append(buf, r)
		default:
			buf = append(buf, '_')
		}
	}
	return string(buf)
}
