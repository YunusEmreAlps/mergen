package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultBootArgs    = "console=ttyS0 reboot=k panic=1 pci=off"
	kernelFileName     = "vmlinux.bin"
	rootfsFileName     = "rootfs.ext4"
	socketFileName     = "firecracker.sock"
	logFifoName        = "firecracker.log.fifo"
	metricsFifoName    = "firecracker.metrics.fifo"
	statusFileName     = "status.json"
	defaultImagesDir   = "images"
	defaultVolumesDir  = "volumes"
	defaultLogsDir     = "logs"
	defaultFirecracker = "firecracker"
)

type RuntimeOptions struct {
	ProjectDir          string
	ImagesDir           string
	VolumesDir          string
	LogsDir             string
	FirecrackerBinary   string
	KernelImagePath     string
	RootfsImagePath     string
	BootArgs            string
	SkipImageValidation bool
}

type Runtime struct {
	ProjectDir        string
	ImagesDir         string
	VolumesDir        string
	LogsDir           string
	FirecrackerBinary string
	KernelImagePath   string
	RootfsImagePath   string
	BootArgs          string
}

func LoadRuntime(opts RuntimeOptions) (Runtime, error) {
	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = os.Getenv("PROJECT_PWD")
	}
	if projectDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Runtime{}, fmt.Errorf("determine project directory: %w", err)
		}
		projectDir = wd
	}

	imagesDir := opts.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(projectDir, defaultImagesDir)
	}
	volumesDir := opts.VolumesDir
	if volumesDir == "" {
		volumesDir = filepath.Join(projectDir, defaultVolumesDir)
	}
	logsDir := opts.LogsDir
	if logsDir == "" {
		logsDir = filepath.Join(projectDir, defaultLogsDir)
	}

	if err := os.MkdirAll(volumesDir, 0o755); err != nil {
		return Runtime{}, fmt.Errorf("create volumes dir: %w", err)
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return Runtime{}, fmt.Errorf("create logs dir: %w", err)
	}

	fcBinary := opts.FirecrackerBinary
	if fcBinary == "" {
		fcBinary = os.Getenv("MERGEN_FIRECRACKER_BIN")
	}
	if fcBinary == "" {
		fcBinary = os.Getenv("FIRECRACKER_BINARY")
	}
	if fcBinary == "" {
		fcBinary = defaultFirecracker
	}

	bootArgs := opts.BootArgs
	if bootArgs == "" {
		bootArgs = DefaultBootArgs
	}

	kernelImagePath := opts.KernelImagePath
	if kernelImagePath == "" {
		kernelImagePath = filepath.Join(imagesDir, kernelFileName)
	}
	rootfsImagePath := opts.RootfsImagePath
	if rootfsImagePath == "" {
		rootfsImagePath = filepath.Join(imagesDir, rootfsFileName)
	}

	runtime := Runtime{
		ProjectDir:        projectDir,
		ImagesDir:         imagesDir,
		VolumesDir:        volumesDir,
		LogsDir:           logsDir,
		FirecrackerBinary: fcBinary,
		KernelImagePath:   kernelImagePath,
		RootfsImagePath:   rootfsImagePath,
		BootArgs:          bootArgs,
	}

	if !opts.SkipImageValidation {
		if err := runtime.validateImages(); err != nil {
			return Runtime{}, err
		}
	}

	return runtime, nil
}

func (r Runtime) validateImages() error {
	if _, err := os.Stat(r.KernelImagePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kernel image %s not found", r.KernelImagePath)
		}
		return fmt.Errorf("check kernel image: %w", err)
	}
	if _, err := os.Stat(r.RootfsImagePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rootfs image %s not found", r.RootfsImagePath)
		}
		return fmt.Errorf("check rootfs image: %w", err)
	}
	return nil
}

type Paths struct {
	ID          string
	SanitizedID string
	VolumeDir   string
	KernelImage string
	RootfsImage string
	SocketPath  string
	LogFifo     string
	MetricsFifo string
	StatusFile  string
	LogFile     string
}

func (r Runtime) MachinePaths(id string) Paths {
	sanitized := SanitizeID(id)
	if sanitized == "" {
		sanitized = "vm"
	}

	volumeDir := filepath.Join(r.VolumesDir, id)
	return Paths{
		ID:          id,
		SanitizedID: sanitized,
		VolumeDir:   volumeDir,
		KernelImage: filepath.Join(volumeDir, kernelFileName),
		RootfsImage: filepath.Join(volumeDir, rootfsFileName),
		SocketPath:  filepath.Join(volumeDir, socketFileName),
		LogFifo:     filepath.Join(volumeDir, logFifoName),
		MetricsFifo: filepath.Join(volumeDir, metricsFifoName),
		StatusFile:  filepath.Join(volumeDir, statusFileName),
		LogFile:     filepath.Join(r.LogsDir, fmt.Sprintf("%s.log", sanitized)),
	}
}
