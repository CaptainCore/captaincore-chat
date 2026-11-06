package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/CaptainCore/captaincore-chat/internal/api"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/spf13/cobra"
)

var configPath string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the chat server",
	Long: `Run the chat relay: websocket at /v1/ws, signed Manager events at
/internal/publish, health at /healthz. Conversations live in the Manager.

Binds to the config's "listen" address (default 127.0.0.1:8490). Put Caddy in
front for TLS. Stops gracefully on SIGINT/SIGTERM.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return api.New(cfg, log).Run(ctx)
	},
}

func init() {
	for _, c := range []*cobra.Command{serveCmd} {
		c.Flags().StringVar(&configPath, "config", defaultConfigPath(), "path to config.json (env CAPTAINCORE_CHAT_CONFIG)")
		rootCmd.AddCommand(c)
	}
}
