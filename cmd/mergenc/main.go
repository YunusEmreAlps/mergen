package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alperreha/mergen/internal/vm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "create":
		runCreate(args)
	case "stop":
		runStop(args)
	case "delete":
		runDelete(args)
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
	fmt.Fprintf(os.Stderr, "  create [runtime flags] [--cpus N] [--memory MB] [--tap-device name] [--host-ip cidr] name\n")
	fmt.Fprintf(os.Stderr, "  stop [runtime flags] [--timeout duration] name\n")
	fmt.Fprintf(os.Stderr, "  delete [runtime flags] [--remove-tap] name\n")
}

type runtimeFlags struct {
	project     *string
	images      *string
	volumes     *string
	logs        *string
	kernel      *string
	rootfs      *string
	bootArgs    *string
	firecracker *string
}

func addRuntimeFlags(fs *flag.FlagSet) runtimeFlags {
	return runtimeFlags{
		project:     fs.String("project", "", "project root containing images, volumes, and logs"),
		images:      fs.String("images", "", "directory containing base images"),
		volumes:     fs.String("volumes", "", "directory for per-machine volumes"),
		logs:        fs.String("logs", "", "directory for machine logs"),
		kernel:      fs.String("kernel", "", "path to kernel image (defaults to images/vmlinux.bin)"),
		rootfs:      fs.String("rootfs", "", "path to rootfs image (defaults to images/rootfs.ext4)"),
		bootArgs:    fs.String("boot-args", vm.DefaultBootArgs, "kernel boot arguments"),
		firecracker: fs.String("firecracker-binary", "", "path to firecracker binary"),
	}
}

func runtimeFromFlags(values runtimeFlags, validateImages bool) (vm.Runtime, error) {
	return vm.LoadRuntime(vm.RuntimeOptions{
		ProjectDir:          *values.project,
		ImagesDir:           *values.images,
		VolumesDir:          *values.volumes,
		LogsDir:             *values.logs,
		KernelImagePath:     *values.kernel,
		RootfsImagePath:     *values.rootfs,
		BootArgs:            *values.bootArgs,
		FirecrackerBinary:   *values.firecracker,
		SkipImageValidation: !validateImages,
	})
}

func runCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	flags := addRuntimeFlags(fs)
	cpus := fs.Int("cpus", 1, "number of virtual CPUs")
	memory := fs.Int("memory", 512, "memory size in MB")
	tap := fs.String("tap-device", "", "tap device to attach to the VM (default tap-<id>)")
	hostIP := fs.String("host-ip", "172.16.0.1/30", "host address (CIDR) to assign to the tap device")
	mac := fs.String("guest-mac", "", "MAC address to assign to the guest NIC")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc create [flags] name\n")
		os.Exit(1)
	}

	name := fs.Arg(0)
	runtime, err := runtimeFromFlags(flags, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime init failed: %v\n", err)
		os.Exit(1)
	}

	tapName := *tap
	if tapName == "" {
		tapName = fmt.Sprintf("tap-%s", vm.SanitizeID(name))
	}

	var network *vm.NetworkOptions
	if tapName != "" {
		network = &vm.NetworkOptions{
			TapDevice:  tapName,
			HostIPCIDR: *hostIP,
			MacAddress: *mac,
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	status, err := vm.Create(ctx, runtime, vm.CreateOptions{
		ID:      name,
		Spec:    vm.Spec{CPUCount: int64(*cpus), MemSizeMb: int64(*memory)},
		Network: network,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s running (socket: %s)\n", status.ID, status.SocketPath)
	if network != nil {
		fmt.Fprintf(os.Stdout, "tap device %s host-ip=%s mac=%s\n", network.TapDevice, network.HostIPCIDR, status.MacAddress)
	}
}

func runStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	flags := addRuntimeFlags(fs)
	timeout := fs.Duration("timeout", 30*time.Second, "time to wait for shutdown")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc stop [flags] name\n")
		os.Exit(1)
	}

	name := fs.Arg(0)
	runtime, err := runtimeFromFlags(flags, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime init failed: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	status, err := vm.Stop(ctx, runtime, name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "machine %s not found\n", name)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "stop failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s status: %s\n", status.ID, status.State)
}

func runDelete(args []string) {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	flags := addRuntimeFlags(fs)
	removeTap := fs.Bool("remove-tap", true, "delete tap device if it was created")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: mergenc delete [flags] name\n")
		os.Exit(1)
	}

	name := fs.Arg(0)
	runtime, err := runtimeFromFlags(flags, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime init failed: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := vm.Delete(ctx, runtime, name, *removeTap); err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "machine %s not found\n", name)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "delete failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "machine %s deleted\n", name)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel
}
