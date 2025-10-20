package controlplane

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	ID                string   `json:"id"`
	KernelImagePath   string   `json:"kernel_image_path"`
	RootDrivePath     string   `json:"root_drive_path"`
	CPUCount          int64    `json:"cpu_count"`
	MemSizeMb         int64    `json:"mem_size_mb"`
	BootArgs          string   `json:"boot_args"`
	SocketPath        string   `json:"socket_path"`
	LogDir            string   `json:"log_dir"`
	ContainerImageURL string   `json:"container_image_url"`
	ContainerCommand  []string `json:"container_command"`
	ContainerEnv      []string `json:"container_env"`
	ContainerWorkDir  string   `json:"container_workdir"`
	FirecrackerBinary string   `json:"firecracker_binary"`
	GuestAddress      string   `json:"guest_address"`
	GuestHTTPPort     int      `json:"guest_http_port"`
	GuestHTTPURL      string   `json:"guest_http_url"`
	StartupScriptID   string   `json:"startup_script_id"`
	StartupScript     string   `json:"-"`
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
	ContainerCommand  []string       `json:"container_command,omitempty"`
	ContainerEnv      []string       `json:"container_env,omitempty"`
	ContainerWorkDir  string         `json:"container_workdir,omitempty"`
	RootDrivePath     string         `json:"root_drive_path,omitempty"`
	GuestAddress      string         `json:"guest_address,omitempty"`
	GuestHTTPPort     int            `json:"guest_http_port,omitempty"`
	GuestHTTPURL      string         `json:"guest_http_url,omitempty"`
	StartupScriptID   string         `json:"startup_script_id,omitempty"`
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
	containerCommand  []string
	containerEnv      []string
	containerWorkDir  string
	guestAddress      string
	guestHTTPPort     int
	guestHTTPURL      string
	kernelCleanup     func()
	rootDrivePath     string
	rootDriveCleanup  func()
	events            []MachineEvent
	eventMu           sync.Mutex
	startupScriptID   string
}

type MachineManager struct {
	mu          sync.RWMutex
	machines    map[string]*MachineRecord
	socketDir   string
	logDir      string
	kernelDir   string
	rootfsDir   string
	defaultBoot string
	fcBinary    string
}

func (m *MachineManager) StreamConsole(ctx context.Context, id string, writer io.Writer) error {
	m.mu.RLock()
	record, ok := m.machines[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("machine %s not found", id)
	}

	if record.logPath == "" {
		return fmt.Errorf("machine %s has no log path", id)
	}

	file, err := os.Open(record.logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer file.Close()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := file.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		return readErr
	}
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
	rootfsDir := filepath.Join(stateDir, "rootfs")

	for _, dir := range []string{stateDir, socketDir, logDir, kernelDir, rootfsDir} {
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
		rootfsDir:   rootfsDir,
		defaultBoot: defaultBootArgs,
		fcBinary:    binary,
	}, nil
}

func (m *MachineManager) CreateAndStart(ctx context.Context, req MachineRequest) (*MachineStatus, error) {
	var err error
	var kernelCleanup func()
	var rootDriveCleanup func()
	defer func() {
		if err != nil && kernelCleanup != nil {
			kernelCleanup()
		}
		if err != nil && rootDriveCleanup != nil {
			rootDriveCleanup()
		}
	}()

	if req.ID == "" {
		return nil, errors.New("id is required")
	}
	if req.KernelImagePath == "" {
		return nil, errors.New("kernel_image_path is required")
	}
	if req.RootDrivePath == "" && req.ContainerImageURL == "" {
		return nil, errors.New("root_drive_path or container_image_url is required")
	}

	if _, err := os.Stat(req.KernelImagePath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
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
		containerCommand:  append([]string(nil), req.ContainerCommand...),
		containerEnv:      append([]string(nil), req.ContainerEnv...),
		containerWorkDir:  req.ContainerWorkDir,
		guestAddress:      req.GuestAddress,
		guestHTTPPort:     req.GuestHTTPPort,
		guestHTTPURL:      computeGuestURL(req.GuestAddress, req.GuestHTTPPort, req.GuestHTTPURL),
		startupScriptID:   req.StartupScriptID,
	}

	rootDrivePath, cleanup, prepErr := m.prepareRootDrive(ctx, record, &req)
	if prepErr != nil {
		record.addEvent("error", fmt.Sprintf("root drive preparation failed: %v", prepErr))
		return nil, wrapMachineError(record, fmt.Errorf("root drive: %w", prepErr))
	}
	rootDriveCleanup = cleanup
	record.rootDrivePath = rootDrivePath
	if _, statErr := os.Stat(rootDrivePath); statErr != nil {
		record.addEvent("error", fmt.Sprintf("root drive check failed: %v", statErr))
		return nil, wrapMachineError(record, fmt.Errorf("root drive: %w", statErr))
	}
	req.RootDrivePath = rootDrivePath
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

	cleanup = func() {
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
	record.rootDriveCleanup = rootDriveCleanup
	m.machines[req.ID] = record
	m.mu.Unlock()

	kernelCleanup = nil
	rootDriveCleanup = nil

	go m.monitorMachine(req.ID, record)

	status := &MachineStatus{
		ID:                req.ID,
		Status:            record.status,
		SocketPath:        socketPath,
		LogPath:           logPath,
		CreatedAt:         record.createdAt,
		PID:               record.pid,
		ContainerImageURL: req.ContainerImageURL,
		ContainerCommand:  append([]string(nil), record.containerCommand...),
		ContainerEnv:      append([]string(nil), record.containerEnv...),
		ContainerWorkDir:  record.containerWorkDir,
		RootDrivePath:     record.rootDrivePath,
		GuestAddress:      record.guestAddress,
		GuestHTTPPort:     record.guestHTTPPort,
		GuestHTTPURL:      record.guestHTTPURL,
		StartupScriptID:   record.startupScriptID,
		Events:            record.snapshotEvents(),
	}

	return status, nil
}

func (m *MachineManager) prepareRootDrive(ctx context.Context, record *MachineRecord, req *MachineRequest) (string, func(), error) {
	if req.RootDrivePath != "" {
		if req.StartupScript != "" {
			record.addEvent("startup_script", "Startup script ignored because a prebuilt root drive was supplied")
		}
		return req.RootDrivePath, nil, nil
	}
	if req.ContainerImageURL == "" {
		return "", nil, errors.New("root_drive_path or container_image_url is required")
	}
	return m.buildRootDriveFromContainer(ctx, record, req)
}

type dockerImageConfig struct {
	Env        []string `json:"Env"`
	Cmd        []string `json:"Cmd"`
	Entrypoint []string `json:"Entrypoint"`
	WorkingDir string   `json:"WorkingDir"`
}

func (m *MachineManager) buildRootDriveFromContainer(ctx context.Context, record *MachineRecord, req *MachineRequest) (string, func(), error) {
	imageRef := req.ContainerImageURL
	record.addEvent("container_image", fmt.Sprintf("Preparing container image %s", imageRef))

	tmpDir, err := os.MkdirTemp("", "mergen-container-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	rootfsDir := filepath.Join(tmpDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("create rootfs dir: %w", err)
	}

	record.addEvent("container_image", "Pulling image from registry")
	if _, err := runCommand(ctx, "docker", "pull", imageRef); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("docker pull: %w", err)
	}

	record.addEvent("container_image", "Inspecting container metadata")
	configJSON, err := runCommand(ctx, "docker", "image", "inspect", "--format", "{{json .Config}}", imageRef)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("docker inspect: %w", err)
	}
	var cfg dockerImageConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("decode docker config: %w", err)
	}

	record.addEvent("container_image", "Creating temporary container")
	containerID, err := runCommand(ctx, "docker", "create", imageRef)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("docker create: %w", err)
	}
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		os.RemoveAll(tmpDir)
		return "", nil, errors.New("docker create returned empty container id")
	}
	defer func() {
		_, _ = runCommand(context.Background(), "docker", "rm", "-f", containerID)
	}()

	tarPath := filepath.Join(tmpDir, "rootfs.tar")
	record.addEvent("container_image", "Exporting filesystem layers")
	if err := exportContainerFilesystem(ctx, containerID, tarPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, err
	}

	record.addEvent("container_image", "Unpacking container filesystem")
	if _, err := runCommand(ctx, "tar", "-C", rootfsDir, "-xf", tarPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("untar rootfs: %w", err)
	}
	_ = os.Remove(tarPath)

	resolvedEnv := mergeEnv(cfg.Env, req.ContainerEnv)
	resolvedCmd := resolveContainerCommand(cfg.Entrypoint, cfg.Cmd, req.ContainerCommand)
	workDir := req.ContainerWorkDir
	if workDir == "" {
		workDir = cfg.WorkingDir
	}

	if req.StartupScript != "" {
		record.addEvent("startup_script", fmt.Sprintf("Embedding startup script %s", describeScriptOrigin(req.StartupScriptID)))
	}
	if err := configureContainerInit(rootfsDir, resolvedCmd, resolvedEnv, workDir, req.StartupScript); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("configure init: %w", err)
	}
	record.containerCommand = append([]string(nil), resolvedCmd...)
	record.containerEnv = append([]string(nil), resolvedEnv...)
	record.containerWorkDir = workDir
	record.addEvent("container_image", fmt.Sprintf("Initialized boot command: %s", strings.Join(quoteArgs(resolvedCmd), " ")))

	sizeBytes, err := directorySize(rootfsDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("rootfs size: %w", err)
	}
	extra := sizeBytes / 2
	const minExtra int64 = 256 * 1024 * 1024
	if extra < minExtra {
		extra = minExtra
	}
	imageSize := alignSize(sizeBytes+extra, 4096)

	outputPath := filepath.Join(m.rootfsDir, fmt.Sprintf("%s.ext4", sanitizeResourceID(req.ID)))
	if err := os.MkdirAll(m.rootfsDir, 0o755); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("create rootfs dir: %w", err)
	}
	_ = os.Remove(outputPath)
	file, err := os.Create(outputPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("create rootfs: %w", err)
	}
	if err := file.Truncate(imageSize); err != nil {
		file.Close()
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("resize rootfs: %w", err)
	}
	file.Close()

	record.addEvent("container_image", fmt.Sprintf("Writing ext4 image (%d MB)", imageSize/1024/1024))
	if _, err := runCommand(ctx, "mke2fs", "-t", "ext4", "-d", rootfsDir, "-F", outputPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("mke2fs: %w", err)
	}

	os.RemoveAll(tmpDir)
	record.addEvent("container_image", fmt.Sprintf("Container root drive ready at %s", outputPath))
	cleanup := func() {
		_ = os.Remove(outputPath)
	}
	return outputPath, cleanup, nil
}

func exportContainerFilesystem(ctx context.Context, containerID, tarPath string) error {
	file, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("create tar: %w", err)
	}
	defer file.Close()

	cmd := exec.CommandContext(ctx, "docker", "export", containerID)
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker export: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func mergeEnv(base, overrides []string) []string {
	index := make(map[string]int)
	var ordered []string
	appendEnv := func(items []string) {
		for _, kv := range items {
			eq := strings.Index(kv, "=")
			if eq <= 0 {
				continue
			}
			key := kv[:eq]
			if pos, ok := index[key]; ok {
				ordered[pos] = kv
				continue
			}
			index[key] = len(ordered)
			ordered = append(ordered, kv)
		}
	}
	appendEnv(base)
	appendEnv(overrides)
	return append([]string(nil), ordered...)
}

func resolveContainerCommand(entrypoint, cmd, override []string) []string {
	if len(override) > 0 {
		return append([]string{}, append(entrypoint, override...)...)
	}
	if len(entrypoint) > 0 {
		return append([]string{}, append(entrypoint, cmd...)...)
	}
	if len(cmd) > 0 {
		return append([]string{}, cmd...)
	}
	return []string{"/bin/sh"}
}

func configureContainerInit(rootfsDir string, command, env []string, workDir string, startupScript string) error {
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}
	mergenDir := filepath.Join(rootfsDir, "etc", "mergen")
	if err := os.MkdirAll(mergenDir, 0o755); err != nil {
		return fmt.Errorf("create mergen dir: %w", err)
	}
	if len(env) > 0 {
		envPath := filepath.Join(mergenDir, "env.sh")
		if err := writeEnvFile(envPath, env); err != nil {
			return err
		}
	}
	if workDir != "" {
		workPath := filepath.Join(mergenDir, "workdir")
		if err := os.WriteFile(workPath, []byte(workDir+"\n"), 0o644); err != nil {
			return fmt.Errorf("write workdir: %w", err)
		}
	}
	if startupScript != "" {
		scriptPath := filepath.Join(mergenDir, "startup.sh")
		contents := startupScript
		if !strings.HasSuffix(contents, "\n") {
			contents += "\n"
		}
		if !strings.HasPrefix(strings.TrimSpace(contents), "#!/") {
			contents = "#!/bin/sh\nset -e\n" + contents
		}
		if err := os.WriteFile(scriptPath, []byte(contents), 0o755); err != nil {
			return fmt.Errorf("write startup script: %w", err)
		}
	}
	sbinDir := filepath.Join(rootfsDir, "sbin")
	if err := os.MkdirAll(sbinDir, 0o755); err != nil {
		return fmt.Errorf("create sbin: %w", err)
	}
	initPath := filepath.Join(sbinDir, "init")
	script := buildInitScript(command, startupScript != "")
	if err := os.WriteFile(initPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write init: %w", err)
	}
	return nil
}

func writeEnvFile(path string, env []string) error {
	var buf bytes.Buffer
	for _, kv := range env {
		eq := strings.Index(kv, "=")
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		val := kv[eq+1:]
		buf.WriteString("export ")
		buf.WriteString(key)
		buf.WriteString("=")
		buf.WriteString(shellQuote(val))
		buf.WriteString("\n")
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func buildInitScript(command []string, hasStartupScript bool) string {
	var buf bytes.Buffer
	buf.WriteString("#!/bin/sh\n")
	buf.WriteString("set -e\n")
	buf.WriteString("if [ -f /etc/profile ]; then\n. /etc/profile\nfi\n")
	buf.WriteString("if [ -f /etc/mergen/env.sh ]; then\n. /etc/mergen/env.sh\nfi\n")
	buf.WriteString("if [ -f /etc/mergen/workdir ]; then\nMERGEN_WORKDIR=$(cat /etc/mergen/workdir)\nif [ -n \"$MERGEN_WORKDIR\" ]; then\ncd \"$MERGEN_WORKDIR\" 2>/dev/null || echo \"mergen-init: failed to cd to $MERGEN_WORKDIR\" >&2\nfi\nfi\n")
	if hasStartupScript {
		buf.WriteString("if [ -x /etc/mergen/startup.sh ]; then\necho \"[mergen] running startup script...\" >&2\n/etc/mergen/startup.sh || echo \"mergen-init: startup script exited with $?.\" >&2\nfi\n")
	}
	buf.WriteString("echo \"[mergen] launching container payload...\" >&2\n")
	buf.WriteString("set --")
	for _, arg := range command {
		buf.WriteByte(' ')
		buf.WriteString(shellQuote(arg))
	}
	buf.WriteString("\nexec \"$@\"\n")
	return buf.String()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	var buf strings.Builder
	buf.WriteByte('\'')
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '\'' {
			buf.WriteString("'\\''")
			continue
		}
		buf.WriteByte(ch)
	}
	buf.WriteByte('\'')
	return buf.String()
}

func quoteArgs(args []string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		result = append(result, shellQuote(arg))
	}
	return result
}

func directorySize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func alignSize(size int64, blockSize int64) int64 {
	if blockSize <= 0 {
		return size
	}
	rem := size % blockSize
	if rem == 0 {
		return size
	}
	return size + (blockSize - rem)
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(buf.String())
		if output != "" {
			return output, fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, output)
		}
		return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return buf.String(), nil
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
		ContainerCommand:  append([]string(nil), record.containerCommand...),
		ContainerEnv:      append([]string(nil), record.containerEnv...),
		ContainerWorkDir:  record.containerWorkDir,
		RootDrivePath:     record.rootDrivePath,
		GuestAddress:      guestAddress,
		GuestHTTPPort:     guestPort,
		GuestHTTPURL:      guestURL,
		StartupScriptID:   record.startupScriptID,
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
	if record.rootDriveCleanup != nil {
		record.rootDriveCleanup()
		record.rootDriveCleanup = nil
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

func describeScriptOrigin(id string) string {
	if id != "" {
		return fmt.Sprintf("from registry entry %s", id)
	}
	return "from inline payload"
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
