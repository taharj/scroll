package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/urfave/cli/v2"

	"scroll-tech/common/database"
	"scroll-tech/common/observability"
	"scroll-tech/common/utils"
	"scroll-tech/common/version"

	"scroll-tech/rollup/internal/config"
	"scroll-tech/rollup/internal/controller/relayer"
	"scroll-tech/rollup/internal/controller/watcher"
	butils "scroll-tech/rollup/internal/utils"
)

var app *cli.App

func init() {
	// Set up rollup-relayer app info.
	app = cli.NewApp()
	app.Action = action
	app.Name = "rollup-relayer"
	app.Usage = "The Scroll Rollup Relayer"
	app.Version = version.Version
	app.Flags = append(app.Flags, utils.CommonFlags...)
	app.Flags = append(app.Flags, utils.RollupRelayerFlags...)
	app.Commands = []*cli.Command{}
	app.Before = func(ctx *cli.Context) error {
		return utils.LogSetup(ctx)
	}
	// Register `rollup-relayer-test` app for integration-test.
	utils.RegisterSimulation(app, utils.RollupRelayerApp)
}

func action(ctx *cli.Context) error {
	// Load config file.
	cfgFile := ctx.String(utils.ConfigFileFlag.Name)
	cfg, err := config.NewConfig(cfgFile)
	if err != nil {
		log.Crit("failed to load config file", "config file", cfgFile, "error", err)
	}

	instanceCtx, instanceCancel := context.WithCancel(ctx.Context)
	loopCtx, loopCancel := context.WithCancel(ctx.Context)
	// Init db connection
	db, err := database.InitDB(cfg.DBConfig)
	if err != nil {
		log.Crit("failed to init db connection", "err", err)
	}

	registry := prometheus.DefaultRegisterer
	observability.Server(ctx, db)

	// Init l2geth connection
	l2client, err := ethclient.Dial(cfg.L2Config.Endpoint)
	if err != nil {
		log.Error("failed to connect l2 geth", "config file", cfgFile, "error", err)
		return err
	}

	initGenesis := ctx.Bool(utils.ImportGenesisFlag.Name)
	l2relayer, err := relayer.NewLayer2Relayer(ctx.Context, l2client, db, cfg.L2Config.RelayerConfig, initGenesis, registry)
	if err != nil {
		log.Error("failed to create l2 relayer", "config file", cfgFile, "error", err)
		return err
	}

	chunkProposer := watcher.NewChunkProposer(instanceCtx, cfg.L2Config.ChunkProposerConfig, db, registry)
	if err != nil {
		log.Error("failed to create chunkProposer", "config file", cfgFile, "error", err)
		return err
	}

	batchProposer := watcher.NewBatchProposer(instanceCtx, cfg.L2Config.BatchProposerConfig, db, registry)
	if err != nil {
		log.Error("failed to create batchProposer", "config file", cfgFile, "error", err)
		return err
	}

	l2watcher := watcher.NewL2WatcherClient(instanceCtx, l2client, cfg.L2Config.Confirmations, cfg.L2Config.L2MessengerAddress,
		cfg.L2Config.L2MessageQueueAddress, cfg.L2Config.WithdrawTrieRootSlot, db, registry)

	// Watcher loop to fetch missing blocks
	go utils.LoopWithContext(loopCtx, 2*time.Second, func(ctx context.Context) {
		number, loopErr := butils.GetLatestConfirmedBlockNumber(ctx, l2client, cfg.L2Config.Confirmations)
		if loopErr != nil {
			log.Error("failed to get block number", "err", loopErr)
			return
		}
		l2watcher.TryFetchRunningMissingBlocks(number)
	})

	// Goroutines that may send transactions periodically.
	go utils.Loop(loopCtx, 2*time.Second, chunkProposer.TryProposeChunk)
	go utils.Loop(loopCtx, 10*time.Second, batchProposer.TryProposeBatch)
	go utils.Loop(loopCtx, 2*time.Second, l2relayer.ProcessPendingBatches)
	go utils.Loop(loopCtx, 15*time.Second, l2relayer.ProcessCommittedBatches)

	defer func() {
		// Initiate the graceful shutdown process.
		log.Info("Graceful shutdown initiated")

		// Prevent new transactions by cancelling the loop context.
		loopCancel()

		// Close relayers to ensure all pending transactions are processed.
		// This includes any in-flight transactions that have not yet been confirmed.
		l2relayer.Close()

		// Halt confirmation signal handling by cancelling the instance context.
		instanceCancel()

		// Close the database connection.
		if err = database.CloseDB(db); err != nil {
			log.Error("Failed to close database connection", "error", err)
		}
	}()

	// Finish start all rollup relayer functions.
	log.Info("Start rollup-relayer successfully")

	// Catch CTRL-C to ensure a graceful shutdown.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Wait until the interrupt signal is received from an OS signal.
	<-interrupt

	return nil
}

// Run rollup relayer cmd instance.
func Run() {
	if err := app.Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
