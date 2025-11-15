package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:          "stop name",
	Short:        "Send Ctrl+Alt+Del to a running microVM",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := managerFromFlags()
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		ctx, cancel := signalContext()
		defer cancel()

		if err := mgr.Stop(ctx, args[0]); err != nil {
			return fmt.Errorf("stop vm: %w", err)
		}

		cmd.Printf("vm %s stopped\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
