package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:          "delete name",
	Short:        "Stop and remove a microVM volume",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := managerFromFlags()
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		ctx, cancel := signalContext()
		defer cancel()

		if err := mgr.Delete(ctx, args[0]); err != nil {
			return fmt.Errorf("delete vm: %w", err)
		}

		cmd.Printf("vm %s deleted\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}
