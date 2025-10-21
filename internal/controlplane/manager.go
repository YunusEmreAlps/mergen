package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBootArgs        = "console=ttyS0 reboot=k panic=1 pci=off"
	defaultStateDirName    = "mergen"
	defaultSocketDirName   = "sockets"
	defaultLogDirName      = "logs"
	defaultFirecrackerBin  = "firecracker"
	socketReadyWaitTimeout = 60 * time.Second
)

type MachineRequest struct {
	ID                string `json:"id"`
	KernelImagePath   string `json:"kernel_image_path"`
	RootDrivePath     string `json:"root_drive_path"`
	CPUCount          int64  `json:"cpu_count"`
	MemSizeMb         int64  `json:"mem_size_mb"`
	BootArgs          string `json:"boot_args"`
	SocketPath        string `json:"socket_path"`
	LogDir            string `json:"log_dir"`
	FirecrackerBinary string `json:"firecracker_binary"`
}

type MachineStatus struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	SocketPath string    `json:"socket_path"`
	LogPath    string    `json:"log_path"`
	CreatedAt  time.Time `json:"created_at"`
	PID        int       `json:"pid"`
	ExitError  string    `json:"exit_error,omitempty"`
}

type MachineRecord struct {
	id         string
	cmd        *exec.Cmd
	socketPath string
	logPath    string
	status     string
	createdAt  time.Time
	pid        int
	exitErr    error
	exitCh     chan struct{}
}

type MachineManager struct {
	mu          sync.RWMutex
	machines    map[string]*MachineRecord
	socketDir   string
	logDir      string
	defaultBoot string
	fcBinary    string
}

func NewMachineManager() (*MachineManager, error) {
	stateDir := os.Getenv("MERGEN_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), defaultStateDirName)
	}
	socketDir := filepath.Join(stateDir, defaultSocketDirName)
	logDir := filepath.Join(stateDir, defaultLogDirName)

	for _, dir := range []string{stateDir, socketDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state dir %s: %w", dir, err)
		}
	}

	binary := os.Getenv("MERGEN_FIRECRACKER_BIN")
	if binary == "" {
		binary = os.Getenv("FIRECRACKER_BINARY")
	}
	if binary == "" {
		binary = defaultFirecrackerBin
	}

	return &MachineManager{
		machines:    make(map[string]*MachineRecord),
		socketDir:   socketDir,
		logDir:      logDir,
		defaultBoot: defaultBootArgs,
		fcBinary:    binary,
	}, nil
}

func (m *MachineManager) CreateAndStart(ctx context.Context, req MachineRequest) (*MachineStatus, error) {
	if req.ID == "" {
		return nil, errors.New("id is required")
	}
	if req.KernelImagePath == "" {
		return nil, errors.New("kernel_image_path is required")
	}
	if req.RootDrivePath == "" {
		return nil, errors.New("root_drive_path is required")
	}
	if _, err := os.Stat(req.KernelImagePath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	if _, err := os.Stat(req.RootDrivePath); err != nil {
		return nil, fmt.Errorf("root drive: %w", err)
	}

	if req.CPUCount <= 0 {
		req.CPUCount = 1
	}
	if req.MemSizeMb <= 0 {
		req.MemSizeMb = 512
	}

	m.mu.Lock()
	if _, exists := m.machines[req.ID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %s already exists", req.ID)
	}
	m.mu.Unlock()

	sanitizedID := sanitizeResourceID(req.ID)
	if sanitizedID == "" {
		sanitizedID = "machine"
	}

	socketPath := req.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(m.socketDir, fmt.Sprintf("firecracker.%s.socket", req.ID))
	}
	if err := ensureParentDir(socketPath); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	_ = os.Remove(socketPath)

	logDir := req.LogDir
	if logDir == "" {
		logDir = m.logDir
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("log dir: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", req.ID))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	binary := req.FirecrackerBinary
	if binary == "" {
		binary = m.fcBinary
	}

	cmd := exec.Command(binary, "--api-sock", socketPath, "--id", sanitizedID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	record := &MachineRecord{
		id:         req.ID,
		cmd:        cmd,
		socketPath: socketPath,
		logPath:    logPath,
		status:     "starting",
		createdAt:  time.Now(),
		pid:        cmd.Process.Pid,
		exitCh:     make(chan struct{}),
	}

	m.mu.Lock()
	m.machines[req.ID] = record
	m.mu.Unlock()

	go m.monitorMachine(req.ID, record)

	if err := waitForSocket(ctx, socketPath, socketReadyWaitTimeout); err != nil {
		m.cleanupMachine(record)
		return nil, fmt.Errorf("wait for socket: %w", err)
	}

	client := newFirecrackerClient(socketPath)
	bootArgs := req.BootArgs
	if bootArgs == "" {
		bootArgs = m.defaultBoot
	}

	if err := client.putMachineConfig(ctx, machineConfigRequest{
		VcpuCount:   req.CPUCount,
		MemSizeMiB:  req.MemSizeMb,
		SMT:         false,
		TrackDirty:  false,
		CpuTemplate: "None",
	}); err != nil {
		m.cleanupMachine(record)
		return nil, fmt.Errorf("machine-config: %w", err)
	}

	if err := client.putBootSource(ctx, bootSourceRequest{
		KernelImagePath: req.KernelImagePath,
		BootArgs:        bootArgs,
	}); err != nil {
		m.cleanupMachine(record)
		return nil, fmt.Errorf("boot-source: %w", err)
	}

	driveID := sanitizeResourceID(fmt.Sprintf("rootfs_%s", req.ID))
	if driveID == "" {
		driveID = "rootfs"
	}
	if err := client.putDrive(ctx, driveID, driveRequest{
		DriveID:      driveID,
		PathOnHost:   req.RootDrivePath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		m.cleanupMachine(record)
		return nil, fmt.Errorf("drive: %w", err)
	}

	if err := client.instanceAction(ctx, actionRequest{ActionType: "InstanceStart"}); err != nil {
		m.cleanupMachine(record)
		return nil, fmt.Errorf("start instance: %w", err)
	}

	record.status = "running"

	status := &MachineStatus{
		ID:         req.ID,
		Status:     record.status,
		SocketPath: socketPath,
		LogPath:    logPath,
		CreatedAt:  record.createdAt,
		PID:        record.pid,
	}
	return status, nil
}

func (m *MachineManager) cleanupMachine(record *MachineRecord) {
	terminateProcess(record.cmd.Process)
	record.cmd.Wait()
	_ = os.Remove(record.socketPath)
	_ = os.Remove(record.logPath)

	m.mu.Lock()
	delete(m.machines, record.id)
	m.mu.Unlock()
}

func (m *MachineManager) DialConsole(ctx context.Context, id string) (net.Conn, error) {
	m.mu.RLock()
	record, ok := m.machines[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("machine %s not found", id)
	}
	d := net.Dialer{}
	return d.DialContext(ctx, "unix", record.socketPath)
}

func (m *MachineManager) monitorMachine(id string, record *MachineRecord) {
	err := record.cmd.Wait()
	m.mu.Lock()
	if existing, ok := m.machines[id]; ok && existing == record {
		record.exitErr = err
		if err != nil {
			record.status = "error"
			log.Printf("machine %s exited with error: %v", id, err)
		} else {
			record.status = "stopped"
			log.Printf("machine %s exited", id)
		}
		record.pid = 0
	}
	m.mu.Unlock()
	close(record.exitCh)
}

func (m *MachineManager) Status(ctx context.Context, id string) (*MachineStatus, error) {
	m.mu.RLock()
	record, ok := m.machines[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("machine %s not found", id)
	}
	status := record.status
	socketPath := record.socketPath
	logPath := record.logPath
	createdAt := record.createdAt
	pid := record.pid
	exitErr := record.exitErr
	m.mu.RUnlock()

	client := newFirecrackerClient(socketPath)
	if info, err := client.instanceInfo(ctx); err == nil {
		status = info.State
		pid = info.PID
	}

	result := &MachineStatus{
		ID:         id,
		Status:     status,
		SocketPath: socketPath,
		LogPath:    logPath,
		CreatedAt:  createdAt,
		PID:        pid,
	}
	if exitErr != nil {
		result.ExitError = exitErr.Error()
	}
	return result, nil
}

func (m *MachineManager) Delete(ctx context.Context, id string) error {
	m.mu.RLock()
	record, ok := m.machines[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("machine %s not found", id)
	}

	client := newFirecrackerClient(record.socketPath)
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.instanceAction(stopCtx, actionRequest{ActionType: "InstanceStop"}); err != nil {
		log.Printf("machine %s stop failed: %v", id, err)
	}

	select {
	case <-record.exitCh:
	case <-time.After(5 * time.Second):
		_ = terminateProcess(record.cmd.Process)
		select {
		case <-record.exitCh:
		case <-time.After(3 * time.Second):
		}
	}

	_ = os.Remove(record.socketPath)
	_ = os.Remove(record.logPath)

	m.mu.Lock()
	delete(m.machines, id)
	m.mu.Unlock()

	return nil
}

type machineConfigRequest struct {
	VcpuCount   int64  `json:"vcpu_count"`
	MemSizeMiB  int64  `json:"mem_size_mib"`
	SMT         bool   `json:"smt"`
	TrackDirty  bool   `json:"track_dirty_pages"`
	CpuTemplate string `json:"cpu_template"`
}

type bootSourceRequest struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type driveRequest struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type actionRequest struct {
	ActionType string `json:"action_type"`
}

type instanceInfoResponse struct {
	State string `json:"state"`
	PID   int    `json:"pid"`
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

func (c *firecrackerClient) putMachineConfig(ctx context.Context, payload machineConfigRequest) error {
	return c.do(ctx, http.MethodPut, "/machine-config", payload)
}

func (c *firecrackerClient) putBootSource(ctx context.Context, payload bootSourceRequest) error {
	return c.do(ctx, http.MethodPut, "/boot-source", payload)
}

func (c *firecrackerClient) putDrive(ctx context.Context, driveID string, payload driveRequest) error {
	path := fmt.Sprintf("/drives/%s", driveID)
	return c.do(ctx, http.MethodPut, path, payload)
}

func (c *firecrackerClient) instanceAction(ctx context.Context, payload actionRequest) error {
	return c.do(ctx, http.MethodPut, "/actions", payload)
}

func (c *firecrackerClient) instanceInfo(ctx context.Context) (*instanceInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/instance-info", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("instance-info failed: %s", string(body))
	}

	var out instanceInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	return &out, nil
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
		return fmt.Errorf("firecracker %s %s failed: %s", method, path, string(data))
	}

	io.Copy(io.Discard, resp.Body)
	return nil
}

func sanitizeResourceID(id string) string {
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

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o755)
}

func waitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timeout waiting for socket")
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err == nil {
				return nil
			}
		}
	}
}

func terminateProcess(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	if err := syscall.Kill(-proc.Pid, syscall.SIGTERM); err != nil {
		if !errors.Is(err, os.ErrProcessDone) && err != syscall.ESRCH {
			return err
		}
	}
	return nil
}
