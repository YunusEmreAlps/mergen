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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

const (
	defaultBootArgs    = "console=ttyS0 reboot=k panic=1 pci=off"
	kernelFileName     = "vmlinux.bin"
	rootfsFileName     = "rootfs.ext4"
	defaultFirecracker = "firecracker"
	defaultStateName   = "state.json"
	defaultVolumesDir  = "volumes"
	defaultImagesDir   = "images"
	defaultLogsDir     = "logs"
	logFifoName        = "firecracker.log.fifo"
	metricsFifoName    = "firecracker.metrics.fifo"
	socketFileName     = "firecracker.sock"
)

type MachineSpec struct {
	CPUCount  int64 `json:"cpu_count"`
	MemSizeMb int64 `json:"mem_size_mb"`
}

type MachineStatus struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	SocketPath string    `json:"socket_path"`
	VolumePath string    `json:"volume_path"`
	CreatedAt  time.Time `json:"created_at"`
}

type machineRecord struct {
	id          string
	machine     *firecracker.Machine
	status      string
	createdAt   time.Time
	socketPath  string
	volumePath  string
	logFifo     string
	metricsFifo string
	monitorCh   chan error
	logFile     *os.File
}

type persistedMachine struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	SocketPath  string    `json:"socket_path"`
	VolumePath  string    `json:"volume_path"`
	LogFifo     string    `json:"log_fifo"`
	MetricsFifo string    `json:"metrics_fifo"`
	CreatedAt   time.Time `json:"created_at"`
}

type stateFile struct {
	Machines map[string]persistedMachine `json:"machines"`
}

type MachineManager struct {
	mu         sync.RWMutex
	machines   map[string]*machineRecord
	bootArgs   string
	fcBinary   string
	imagesDir  string
	volumesDir string
	logsDir    string
	statePath  string
}

func NewMachineManager() (*MachineManager, error) {
	projectDir := os.Getenv("PROJECT_PWD")
	if projectDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determine project directory: %w", err)
		}
		projectDir = wd
	}

	imagesDir := filepath.Join(projectDir, defaultImagesDir)
	volumesDir := filepath.Join(projectDir, defaultVolumesDir)
	logsDir := filepath.Join(projectDir, defaultLogsDir)
	if err := os.MkdirAll(volumesDir, 0o755); err != nil {
		return nil, fmt.Errorf("create volumes dir: %w", err)
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}

	statePath := filepath.Join(projectDir, defaultStateName)

	fcBinary := os.Getenv("MERGEN_FIRECRACKER_BIN")
	if fcBinary == "" {
		fcBinary = os.Getenv("FIRECRACKER_BINARY")
	}
	if fcBinary == "" {
		fcBinary = defaultFirecracker
	}

	mgr := &MachineManager{
		machines:   make(map[string]*machineRecord),
		bootArgs:   defaultBootArgs,
		fcBinary:   fcBinary,
		imagesDir:  imagesDir,
		volumesDir: volumesDir,
		logsDir:    logsDir,
		statePath:  statePath,
	}

	if err := mgr.loadState(); err != nil {
		return nil, err
	}

	return mgr, nil
}

func (m *MachineManager) loadState() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state file: %w", err)
	}

	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}

	for id, persisted := range state.Machines {
		rec := &machineRecord{
			id:          id,
			status:      persisted.Status,
			createdAt:   persisted.CreatedAt,
			socketPath:  persisted.SocketPath,
			volumePath:  persisted.VolumePath,
			logFifo:     persisted.LogFifo,
			metricsFifo: persisted.MetricsFifo,
			monitorCh:   make(chan error, 1),
		}
		m.machines[id] = rec
	}
	return nil
}

func (m *MachineManager) saveStateLocked() error {
	state := stateFile{Machines: make(map[string]persistedMachine, len(m.machines))}
	for id, rec := range m.machines {
		state.Machines[id] = persistedMachine{
			ID:          id,
			Status:      rec.status,
			SocketPath:  rec.socketPath,
			VolumePath:  rec.volumePath,
			LogFifo:     rec.logFifo,
			MetricsFifo: rec.metricsFifo,
			CreatedAt:   rec.createdAt,
		}
	}

	tmpPath := m.statePath + ".tmp"
	buf, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(tmpPath, buf, 0o644); err != nil {
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, m.statePath); err != nil {
		return fmt.Errorf("commit state file: %w", err)
	}
	return nil
}

func (m *MachineManager) Create(ctx context.Context, id string, spec MachineSpec) (*MachineStatus, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("id is required")
	}
	if spec.CPUCount <= 0 {
		spec.CPUCount = 1
	}
	if spec.MemSizeMb <= 0 {
		spec.MemSizeMb = 512
	}

	sanitized := sanitizeResourceID(id)
	if sanitized == "" {
		sanitized = "vm"
	}

	m.mu.Lock()
	if _, exists := m.machines[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %s already exists", id)
	}
	m.mu.Unlock()

	kernelSrc := filepath.Join(m.imagesDir, kernelFileName)
	rootfsSrc := filepath.Join(m.imagesDir, rootfsFileName)
	if _, err := os.Stat(kernelSrc); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	if _, err := os.Stat(rootfsSrc); err != nil {
		return nil, fmt.Errorf("rootfs image: %w", err)
	}

	volumeDir := filepath.Join(m.volumesDir, id)
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create volume dir: %w", err)
	}

	kernelDst := filepath.Join(volumeDir, kernelFileName)
	rootfsDst := filepath.Join(volumeDir, rootfsFileName)
	if err := copyFile(kernelSrc, kernelDst); err != nil {
		return nil, fmt.Errorf("copy kernel: %w", err)
	}
	if err := copyFile(rootfsSrc, rootfsDst); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	socketPath := filepath.Join(volumeDir, socketFileName)
	logFifo := filepath.Join(volumeDir, logFifoName)
	metricsFifo := filepath.Join(volumeDir, metricsFifoName)
	_ = os.Remove(socketPath)
	_ = os.Remove(logFifo)
	_ = os.Remove(metricsFifo)

	if err := makeFifo(logFifo); err != nil {
		return nil, fmt.Errorf("create log fifo: %w", err)
	}
	if err := makeFifo(metricsFifo); err != nil {
		return nil, fmt.Errorf("create metrics fifo: %w", err)
	}

	logFilePath := filepath.Join(m.logsDir, fmt.Sprintf("%s.log", sanitized))
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	logger := logrus.New()
	logger.SetOutput(logFile)
	logger.SetLevel(logrus.InfoLevel)

	cfg := firecracker.Config{
		SocketPath:      socketPath,
		LogFifo:         logFifo,
		MetricsFifo:     metricsFifo,
		KernelImagePath: kernelDst,
		KernelArgs:      m.bootArgs,
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String(fmt.Sprintf("rootfs_%s", sanitized)),
				PathOnHost:   firecracker.String(rootfsDst),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       firecracker.Int64(spec.CPUCount),
			MemSizeMib:      firecracker.Int64(spec.MemSizeMb),
			Smt:             firecracker.Bool(false),
			TrackDirtyPages: firecracker.Bool(false),
		},
		VmID: sanitized,
	}

	machineOpts := []firecracker.Opt{
		firecracker.WithProcessRunner(firecracker.NewDefaultProcessRunner(m.fcBinary, logFile, logFile)),
		firecracker.WithLogger(logrus.NewEntry(logger)),
	}

	machine, err := firecracker.NewMachine(ctx, cfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("init machine: %w", err)
	}

	if err := machine.Start(ctx); err != nil {
		return nil, fmt.Errorf("start machine: %w", err)
	}

	go streamFIFO(logFifo, logFilePath)
	go streamFIFO(metricsFifo, "")

	rec := &machineRecord{
		id:          id,
		machine:     machine,
		status:      "running",
		createdAt:   time.Now(),
		socketPath:  socketPath,
		volumePath:  volumeDir,
		logFifo:     logFifo,
		metricsFifo: metricsFifo,
		monitorCh:   make(chan error, 1),
		logFile:     logFile,
	}

	go m.monitorMachine(context.Background(), rec)

	m.mu.Lock()
	m.machines[id] = rec
	if err := m.saveStateLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()

	status := &MachineStatus{
		ID:         id,
		Status:     rec.status,
		SocketPath: rec.socketPath,
		VolumePath: rec.volumePath,
		CreatedAt:  rec.createdAt,
	}
	return status, nil
}

func (m *MachineManager) Stop(ctx context.Context, id string) (*MachineStatus, error) {
	m.mu.RLock()
	rec, ok := m.machines[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("machine %s not found", id)
	}

	client := newFirecrackerClient(rec.socketPath)
	if err := client.instanceAction(ctx, actionRequest{ActionType: "InstanceStop"}); err != nil {
		log.Printf("stop request failed for %s: %v", id, err)
	}

	if rec.machine != nil {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := rec.machine.Shutdown(stopCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("machine %s shutdown error: %v", id, err)
		}
	}

	select {
	case <-rec.monitorCh:
	case <-time.After(5 * time.Second):
	}

	m.mu.Lock()
	rec.status = "stopped"
	if err := m.saveStateLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()

	status := &MachineStatus{
		ID:         id,
		Status:     rec.status,
		SocketPath: rec.socketPath,
		VolumePath: rec.volumePath,
		CreatedAt:  rec.createdAt,
	}
	return status, nil
}

func (m *MachineManager) Delete(ctx context.Context, id string) error {
	status, err := m.Stop(ctx, id)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	_ = status

	m.mu.Lock()
	rec, ok := m.machines[id]
	if ok {
		delete(m.machines, id)
	}
	if err := m.saveStateLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	if ok {
		_ = os.Remove(rec.socketPath)
		_ = os.Remove(rec.logFifo)
		_ = os.Remove(rec.metricsFifo)
		if rec.volumePath != "" {
			_ = os.RemoveAll(rec.volumePath)
		}
	}
	return nil
}

func (m *MachineManager) monitorMachine(ctx context.Context, rec *machineRecord) {
	if rec.machine == nil {
		return
	}
	err := rec.machine.Wait(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("machine %s exited with error: %v", rec.id, err)
	}
	rec.monitorCh <- err
	close(rec.monitorCh)
	if rec.logFile != nil {
		_ = rec.logFile.Close()
	}

	m.mu.Lock()
	if existing, ok := m.machines[rec.id]; ok && existing == rec {
		if err != nil {
			rec.status = "error"
		} else {
			rec.status = "stopped"
		}
		_ = m.saveStateLocked()
	}
	m.mu.Unlock()
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
		log.Printf("open fifo %s failed: %v", path, err)
		return
	}
	defer file.Close()

	var writer io.Writer
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("open log file %s failed: %v", logPath, err)
			writer = io.Discard
		} else {
			defer f.Close()
			writer = f
		}
	} else {
		writer = io.Discard
	}

	if _, err := io.Copy(writer, file); err != nil {
		log.Printf("copy fifo %s failed: %v", path, err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

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
