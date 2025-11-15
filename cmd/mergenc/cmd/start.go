package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:          "start name",
	Short:        "Start a microVM using its stored configuration",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := managerFromFlags()
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		ctx, cancel := signalContext()
		defer cancel()

		if err := mgr.Start(ctx, args[0]); err != nil {
			return fmt.Errorf("start vm: %w", err)
		}

		cfg, err := mgr.LoadConfig(args[0])
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		cmd.Printf("vm %s started (socket: %s)\n", cfg.Name, cfg.Paths.Socket)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
