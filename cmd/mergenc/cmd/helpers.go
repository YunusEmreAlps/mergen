package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/alperreha/mergen/internal/vm"
)

func managerFromFlags() (*vm.Manager, error) {
	return vm.NewManager(vm.ManagerConfig{
		ImagesDir:       imagesDirFlag,
		VolumesDir:      volumesDirFlag,
		FirecrackerPath: firecrackerBinFlag,
	})
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
