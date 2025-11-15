package cmd

import (
	"fmt"

	"github.com/alperreha/mergen/internal/vm"
	"github.com/spf13/cobra"
)

const defaultBridge = "docker0"

var (
	createCPU     int
	createMemory  int
	createGuestIP string
	createGateway string
	createNetmask string
	createBridge  string
	createTap     string
	createMAC     string
)

var createCmd = &cobra.Command{
	Use:          "create name",
	Short:        "Prepare a microVM volume from image templates",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := managerFromFlags()
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		ctx, cancel := signalContext()
		defer cancel()

		cfg, err := mgr.Create(ctx, vm.CreateOptions{
			Name:      args[0],
			CPUCount:  createCPU,
			MemoryMB:  createMemory,
			GuestIP:   createGuestIP,
			GatewayIP: createGateway,
			Netmask:   createNetmask,
			Bridge:    createBridge,
			TapDevice: createTap,
			MacAddr:   createMAC,
		})
		if err != nil {
			return fmt.Errorf("create vm: %w", err)
		}

		cmd.Printf("vm %s prepared\n", cfg.Name)
		cmd.Printf(" socket: %s\n", cfg.Paths.Socket)
		cmd.Printf(" config: %s\n", cfg.Paths.Config)
		cmd.Printf(" tap: %s bridge: %s ip: %s gateway: %s mask: %s mac: %s\n",
			cfg.Network.TapDevice,
			cfg.Network.Bridge,
			cfg.Network.GuestIP,
			cfg.Network.GatewayIP,
			cfg.Network.Netmask,
			cfg.Network.MacAddr,
		)

		return nil
	},
}

func init() {
	createCmd.Flags().IntVar(&createCPU, "cpus", 1, "number of virtual CPUs")
	createCmd.Flags().IntVar(&createMemory, "memory", 512, "memory size in MiB")
	createCmd.Flags().StringVar(&createGuestIP, "guest-ip", "", "static IP address to assign inside the guest (required)")
	createCmd.Flags().StringVar(&createGateway, "gateway-ip", "", "gateway IP address (defaults to bridge IPv4 address)")
	createCmd.Flags().StringVar(&createNetmask, "netmask", "", "netmask in dotted decimal (defaults to bridge netmask)")
	createCmd.Flags().StringVar(&createBridge, "bridge", defaultBridge, "bridge interface to attach the tap device")
	createCmd.Flags().StringVar(&createTap, "tap", "", "tap device name (defaults to fc-<name>-tap0)")
	createCmd.Flags().StringVar(&createMAC, "mac", "", "override guest MAC address")
	createCmd.MarkFlagRequired("guest-ip")
	rootCmd.AddCommand(createCmd)
}
