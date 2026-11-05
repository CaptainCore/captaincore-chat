package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/CaptainCore/captaincore-chat/internal/api"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/CaptainCore/captaincore-chat/internal/store"
	"github.com/spf13/cobra"
)

var configPath string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the chat server",
	Long: `Run the chat server: REST API under /v1, websocket at /v1/ws, health at /healthz.

Binds to the config's "listen" address (default 127.0.0.1:8490). Put Caddy in
front for TLS. Stops gracefully on SIGINT/SIGTERM.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		st, err := store.Open(cfg.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return api.New(cfg, st, log).Run(ctx)
	},
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Create or upgrade the database, then exit",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		st, err := store.Open(cfg.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		v, err := st.SchemaVersion(cmd.Context())
		if err != nil {
			return err
		}
		fmt.Printf("%s at schema %d\n", cfg.Database, v)
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{serveCmd, migrateCmd} {
		c.Flags().StringVar(&configPath, "config", defaultConfigPath(), "path to config.json (env CAPTAINCORE_CHAT_CONFIG)")
		rootCmd.AddCommand(c)
	}
}
