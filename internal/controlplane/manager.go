package controlplane

import (
	"bytes"
	"compress/gzip"
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
	ContainerImageURL string `json:"container_image_url"`
	FirecrackerBinary string `json:"firecracker_binary"`
	GuestAddress      string `json:"guest_address"`
	GuestHTTPPort     int    `json:"guest_http_port"`
	GuestHTTPURL      string `json:"guest_http_url"`
}

type MachineStatus struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	SocketPath        string         `json:"socket_path"`
	LogPath           string         `json:"log_path"`
	CreatedAt         time.Time      `json:"created_at"`
	PID               int            `json:"pid"`
	ExitError         string         `json:"exit_error,omitempty"`
	ContainerImageURL string         `json:"container_image_url,omitempty"`
	GuestAddress      string         `json:"guest_address,omitempty"`
	GuestHTTPPort     int            `json:"guest_http_port,omitempty"`
	GuestHTTPURL      string         `json:"guest_http_url,omitempty"`
	Events            []MachineEvent `json:"events,omitempty"`
}

type MachineEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Stage     string    `json:"stage"`
	Message   string    `json:"message"`
}

type MachineError struct {
	Err    error
	Events []MachineEvent
}

func (e *MachineError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *MachineError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type MachineRecord struct {
	id                string
	cmd               *exec.Cmd
	socketPath        string
	logPath           string
	status            string
	createdAt         time.Time
	pid               int
	exitErr           error
	exitCh            chan struct{}
	containerImageURL string
	guestAddress      string
	guestHTTPPort     int
	guestHTTPURL      string
	kernelCleanup     func()
	events            []MachineEvent
	eventMu           sync.Mutex
}

type MachineManager struct {
	mu          sync.RWMutex
	machines    map[string]*MachineRecord
	socketDir   string
	logDir      string
	kernelDir   string
	defaultBoot string
	fcBinary    string
}

func (r *MachineRecord) addEvent(stage, message string) {
	event := MachineEvent{
		Timestamp: time.Now(),
		Stage:     stage,
		Message:   message,
	}
	r.eventMu.Lock()
	r.events = append(r.events, event)
	r.eventMu.Unlock()
	if r.id != "" {
		log.Printf("machine %s: [%s] %s", r.id, stage, message)
	} else {
		log.Printf("machine: [%s] %s", stage, message)
	}
}

func (r *MachineRecord) snapshotEvents() []MachineEvent {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	events := make([]MachineEvent, len(r.events))
	copy(events, r.events)
	return events
}

func wrapMachineError(record *MachineRecord, err error) error {
	if err == nil {
		return nil
	}
	if record == nil {
		return err
	}
	return &MachineError{Err: err, Events: record.snapshotEvents()}
}

func NewMachineManager() (*MachineManager, error) {
	stateDir := os.Getenv("MERGEN_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), defaultStateDirName)
	}
	socketDir := filepath.Join(stateDir, defaultSocketDirName)
	logDir := filepath.Join(stateDir, defaultLogDirName)

	kernelDir := filepath.Join(stateDir, "kernels")

	for _, dir := range []string{stateDir, socketDir, logDir, kernelDir} {
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
		kernelDir:   kernelDir,
		defaultBoot: defaultBootArgs,
		fcBinary:    binary,
	}, nil
}

func (m *MachineManager) CreateAndStart(ctx context.Context, req MachineRequest) (*MachineStatus, error) {
	var err error
	var kernelCleanup func()
	defer func() {
		if err != nil && kernelCleanup != nil {
			kernelCleanup()
		}
	}()

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

	m.mu.RLock()
	if _, exists := m.machines[req.ID]; exists {
		m.mu.RUnlock()
		return nil, fmt.Errorf("machine %s already exists", req.ID)
	}
	m.mu.RUnlock()

	sanitizedID := sanitizeResourceID(req.ID)
	if sanitizedID == "" {
		sanitizedID = "machine"
	}

	socketPath := req.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(m.socketDir, fmt.Sprintf("%s.sock", req.ID))
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

	record := &MachineRecord{
		id:                req.ID,
		socketPath:        socketPath,
		logPath:           logPath,
		status:            "creating",
		createdAt:         time.Now(),
		exitCh:            make(chan struct{}),
		containerImageURL: req.ContainerImageURL,
		guestAddress:      req.GuestAddress,
		guestHTTPPort:     req.GuestHTTPPort,
		guestHTTPURL:      computeGuestURL(req.GuestAddress, req.GuestHTTPPort, req.GuestHTTPURL),
	}
	record.addEvent("creating", "Launching Firecracker process")

	binary := req.FirecrackerBinary
	if binary == "" {
		binary = m.fcBinary
	}

	cmd := exec.Command(binary, "--api-sock", socketPath, "--id", sanitizedID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		record.addEvent("error", fmt.Sprintf("Failed to start Firecracker: %v", err))
		logFile.Close()
		return nil, wrapMachineError(record, fmt.Errorf("start firecracker: %w", err))
	}
	logFile.Close()

	record.cmd = cmd
	record.pid = cmd.Process.Pid

	cleanup := func() {
		_ = terminateProcess(cmd.Process)
		_, _ = cmd.Process.Wait()
		_ = os.Remove(socketPath)
	}

	record.addEvent("waiting_for_socket", fmt.Sprintf("Waiting up to %s for Firecracker socket at %s", socketReadyWaitTimeout, socketPath))
	if err := waitForSocket(ctx, socketPath, socketReadyWaitTimeout); err != nil {
		record.addEvent("error", fmt.Sprintf("Socket wait failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("wait for socket: %w", err))
	}
	record.addEvent("waiting_for_socket", "Firecracker socket detected")

	record.addEvent("configuring", "Preparing kernel image")
	kernelPath := req.KernelImagePath
	kernelPath, kernelCleanup, err = m.prepareKernelImage(kernelPath)
	if err != nil {
		record.addEvent("error", fmt.Sprintf("kernel image prepare failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("kernel image: %w", err))
	}

	client := newFirecrackerClient(socketPath)
	bootArgs := req.BootArgs
	if bootArgs == "" {
		bootArgs = m.defaultBoot
	}

	record.addEvent("configuring", "Pushing machine configuration")
	if err := client.putMachineConfig(ctx, machineConfigRequest{
		VcpuCount:   req.CPUCount,
		MemSizeMiB:  req.MemSizeMb,
		SMT:         false,
		TrackDirty:  false,
		CpuTemplate: "None",
	}); err != nil {
		record.addEvent("error", fmt.Sprintf("machine-config failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("machine-config: %w", err))
	}

	record.addEvent("configuring", "Setting boot source")
	if err := client.putBootSource(ctx, bootSourceRequest{
		KernelImagePath: kernelPath,
		BootArgs:        bootArgs,
	}); err != nil {
		record.addEvent("error", fmt.Sprintf("boot-source failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("boot-source: %w", err))
	}

	driveID := sanitizeResourceID(fmt.Sprintf("rootfs_%s", req.ID))
	record.addEvent("configuring", fmt.Sprintf("Attaching root drive %s", req.RootDrivePath))
	if err := client.putDrive(ctx, driveID, driveRequest{
		DriveID:      driveID,
		PathOnHost:   req.RootDrivePath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		record.addEvent("error", fmt.Sprintf("drive attachment failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("drive: %w", err))
	}

	record.addEvent("starting", "Issuing InstanceStart action")
	if err := client.instanceAction(ctx, actionRequest{ActionType: "InstanceStart"}); err != nil {
		record.addEvent("error", fmt.Sprintf("instance start failed: %v", err))
		cleanup()
		return nil, wrapMachineError(record, fmt.Errorf("start instance: %w", err))
	}

	record.status = "running"
	record.addEvent("running", "MicroVM started successfully")

	info, infoErr := client.instanceInfo(ctx)
	if infoErr == nil {
		record.status = info.State
		record.pid = info.PID
	}

	m.mu.Lock()
	record.kernelCleanup = kernelCleanup
	m.machines[req.ID] = record
	m.mu.Unlock()

	kernelCleanup = nil

	go m.monitorMachine(req.ID, record)

	status := &MachineStatus{
		ID:                req.ID,
		Status:            record.status,
		SocketPath:        socketPath,
		LogPath:           logPath,
		CreatedAt:         record.createdAt,
		PID:               record.pid,
		ContainerImageURL: req.ContainerImageURL,
		GuestAddress:      record.guestAddress,
		GuestHTTPPort:     record.guestHTTPPort,
		GuestHTTPURL:      record.guestHTTPURL,
		Events:            record.snapshotEvents(),
	}

	return status, nil
}

func (m *MachineManager) monitorMachine(id string, record *MachineRecord) {
	err := record.cmd.Wait()
	m.mu.Lock()
	if existing, ok := m.machines[id]; ok && existing == record {
		record.exitErr = err
		if err != nil {
			record.status = "error"
			record.addEvent("error", fmt.Sprintf("Firecracker exited with error: %v", err))
		} else {
			record.status = "stopped"
			record.addEvent("stopped", "Firecracker process exited cleanly")
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
	imageURL := record.containerImageURL
	guestAddress := record.guestAddress
	guestPort := record.guestHTTPPort
	guestURL := record.guestHTTPURL
	events := record.snapshotEvents()
	m.mu.RUnlock()

	client := newFirecrackerClient(socketPath)
	if info, err := client.instanceInfo(ctx); err == nil {
		status = info.State
		pid = info.PID
	}

	result := &MachineStatus{
		ID:                id,
		Status:            status,
		SocketPath:        socketPath,
		LogPath:           logPath,
		CreatedAt:         createdAt,
		PID:               pid,
		ContainerImageURL: imageURL,
		GuestAddress:      guestAddress,
		GuestHTTPPort:     guestPort,
		GuestHTTPURL:      guestURL,
		Events:            events,
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
	record.addEvent("deleting", "Requesting microVM shutdown")
	if err := client.instanceAction(stopCtx, actionRequest{ActionType: "InstanceStop"}); err != nil {
		record.addEvent("error", fmt.Sprintf("instance stop failed: %v", err))
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
	if record.kernelCleanup != nil {
		record.kernelCleanup()
	}

	m.mu.Lock()
	delete(m.machines, id)
	m.mu.Unlock()

	record.addEvent("deleted", "Machine resources cleaned up")

	return nil
}

func (m *MachineManager) prepareKernelImage(path string) (string, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()

	header := make([]byte, 4)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", nil, err
	}

	if header[0] == 0x7f && header[1] == 'E' && header[2] == 'L' && header[3] == 'F' {
		return path, nil, nil
	}

	if header[0] != 0x1f || header[1] != 0x8b {
		return "", nil, fmt.Errorf("unsupported kernel format: expected ELF or gzip-compressed ELF")
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", nil, err
	}

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return "", nil, err
	}
	defer gzReader.Close()

	if err := os.MkdirAll(m.kernelDir, 0o755); err != nil {
		return "", nil, err
	}

	tempFile, err := os.CreateTemp(m.kernelDir, "kernel-*.bin")
	if err != nil {
		return "", nil, err
	}

	if _, err := io.Copy(tempFile, gzReader); err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return "", nil, err
	}

	if err := tempFile.Close(); err != nil {
		os.Remove(tempFile.Name())
		return "", nil, err
	}

	cleanup := func() {
		_ = os.Remove(tempFile.Name())
	}

	return tempFile.Name(), cleanup, nil
}

func computeGuestURL(address string, port int, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if address == "" {
		return ""
	}
	if port <= 0 {
		port = 80
	}
	return fmt.Sprintf("http://%s:%d", address, port)
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

type firecrackerClient struct {
	httpClient *http.Client
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
