package cmd

import "github.com/spf13/cobra"

var (
	imagesDirFlag      string
	volumesDirFlag     string
	firecrackerBinFlag string
)

var rootCmd = &cobra.Command{
	Use:          "mergenc",
	Short:        "Mergen CLI for managing Firecracker microVMs",
	SilenceUsage: true,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&imagesDirFlag, "images-dir", "images", "path to template images directory")
	rootCmd.PersistentFlags().StringVar(&volumesDirFlag, "volumes-dir", "volumes", "path to VM volumes directory")
	rootCmd.PersistentFlags().StringVar(&firecrackerBinFlag, "firecracker-bin", "firecracker", "firecracker binary to execute")
}
