package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/backup"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/logger"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the pmcluster HTTP daemon (REST API + webhook receiver)",
	Long: `Starts the long-running pmcluster daemon. Listens on 127.0.0.1:9090 by
default; Traefik (running in the swarm) routes pmcluster.<domain> to it via
host.docker.internal:9090.

The data directory ($HOME/.pmcluster by default) must already be initialised
via 'pmcluster init'.`,
	RunE: runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — run `pmcluster init` first", cfg.DataDir)
	}

	log, logCloser, err := logger.New(logger.Options{
		LogsDir: cfg.LogsDir(),
		Level:   cfg.LogLevel,
		Console: true,
	})
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logCloser.Close() }()

	if err := logger.Sweep(cfg.LogsDir(), time.Now()); err != nil {
		log.Warn().Err(err).Msg("log sweep had issues")
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// Best-effort: daemon must start even when /var/run/docker.sock is
	// temporarily missing. /api/cluster/info is omitted in that case.
	dc, dockerErr := docker.New()
	if dockerErr != nil {
		log.Warn().Err(dockerErr).Msg("docker client init failed; /api/cluster/info disabled")
	} else {
		defer func() { _ = dc.Close() }()
	}

	if err := replayRegistryLogins(cmd.Context(), st, cfg, log); err != nil {
		log.Warn().Err(err).Msg("registry re-login had issues; private images may fail to pull")
	}

	deployer := cluster.NewDockerCLIDeployer(cmd.OutOrStdout())
	deploySvc := &deploy.Service{Store: st, Deployer: deployer, Backup: backup.LocalTrigger{}}

	cipher, cipherErr := credentials.Open(cfg.EncryptionKeyPath())
	if cipherErr != nil {
		log.Warn().Err(cipherErr).Msg("encryption key not available; /webhook/* disabled")
	}

	handler := server.New(server.Deps{
		Lookup:        st,
		Docker:        dc,
		Store:         st,
		DeployService: deploySvc,
		Cipher:        cipher,
		BackupTrigger: backup.LocalTrigger{},
	})

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info().Str("addr", cfg.ListenAddr).Msg("pmcluster serve listening")
	if err := server.Run(ctx, cfg.ListenAddr, handler); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info().Msg("pmcluster serve stopped cleanly")
	return nil
}
