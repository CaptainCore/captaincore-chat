package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/spf13/cobra"
)

// Development helpers. In production the Manager generates the keypair and
// signs tokens; the chat server never holds a private key.

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Development helpers (keypair, test tokens)",
}

var devKeysOut string

var devKeysCmd = &cobra.Command{
	Use:   "keys",
	Short: "Generate an Ed25519 keypair for local testing",
	Long: `Writes the private key (base64, 64 bytes: the same layout as PHP's
sodium_crypto_sign_secretkey) to --out with mode 0600 and prints the public
key for config.json's public_keys.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(devKeysOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write %s (refusing to overwrite an existing key): %w", devKeysOut, err)
		}
		defer f.Close()
		if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv) + "\n"); err != nil {
			return err
		}
		fmt.Printf("private key written to %s\npublic key:  %s\n", devKeysOut, base64.StdEncoding.EncodeToString(pub))
		return nil
	},
}

var tok struct {
	key, role, name, email, accounts, sites string
	sub                                     int64
	ttl                                     time.Duration
}

var devTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Mint a test token signed with a dev private key",
	Example: `  captaincore-chat dev token --key dev.key --sub 42 --role customer --name Priya --sites 11,12
  captaincore-chat dev token --key dev.key --sub 3 --role operator --name Austin`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return fmt.Errorf("issuer and audience come from the config: %w", err)
		}
		raw, err := os.ReadFile(tok.key)
		if err != nil {
			return err
		}
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(k) != ed25519.PrivateKeySize {
			return errors.New("key file must hold a base64 64-byte Ed25519 private key")
		}
		accounts, err := ids(tok.accounts)
		if err != nil {
			return fmt.Errorf("--accounts: %w", err)
		}
		sites, err := ids(tok.sites)
		if err != nil {
			return fmt.Errorf("--sites: %w", err)
		}
		jti := make([]byte, 8)
		rand.Read(jti)
		now := time.Now()
		t, err := auth.Sign(ed25519.PrivateKey(k), &auth.Claims{
			Issuer: cfg.Issuer, Audience: cfg.Audience, Subject: tok.sub, Name: tok.name, Email: tok.email, Role: tok.role,
			Accounts: accounts, Sites: sites, IssuedAt: now.Unix(), Expires: now.Add(tok.ttl).Unix(), ID: hex.EncodeToString(jti),
		})
		if err != nil {
			return err
		}
		fmt.Println(t)
		return nil
	},
}

func ids(s string) ([]int64, error) {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad id %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func init() {
	devKeysCmd.Flags().StringVar(&devKeysOut, "out", "dev.key", "where to write the private key")
	f := devTokenCmd.Flags()
	f.StringVar(&configPath, "config", defaultConfigPath(), "path to config.json")
	f.StringVar(&tok.key, "key", "dev.key", "private key file from `dev keys`")
	f.Int64Var(&tok.sub, "sub", 0, "Manager user ID")
	f.StringVar(&tok.role, "role", "customer", "customer or operator")
	f.StringVar(&tok.name, "name", "", "display name")
	f.StringVar(&tok.email, "email", "", "email")
	f.StringVar(&tok.accounts, "accounts", "", "comma-separated account IDs")
	f.StringVar(&tok.sites, "sites", "", "comma-separated site IDs")
	f.DurationVar(&tok.ttl, "ttl", 10*time.Minute, "lifetime (max 15m)")
	devTokenCmd.MarkFlagRequired("sub")
	devCmd.AddCommand(devKeysCmd, devTokenCmd)
	rootCmd.AddCommand(devCmd)
}
