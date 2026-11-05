// Package cmd holds the captaincore-chat command line.
package cmd

import (
	"fmt"
	"os"

	"github.com/CaptainCore/captaincore-chat/version"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "captaincore-chat",
	Short:        "Live chat for CaptainCore: customers and operators, inside the Manager",
	SilenceUsage: true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Run: func(cmd *cobra.Command, args []string) {
		// The first line stays "captaincore-chat X.Y.Z": a future upgrader
		// sanity-runs a new binary on it, like the CaptainCore CLI does.
		fmt.Printf("captaincore-chat %s\n", version.Version)
	},
}

// Execute runs the root command.
func Execute() {
	rootCmd.AddCommand(versionCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("CAPTAINCORE_CHAT_CONFIG"); p != "" {
		return p
	}
	return "config.json"
}
