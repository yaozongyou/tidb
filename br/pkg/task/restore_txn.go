package task

import (
	"context"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/restore"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/br/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// RestoreTxnConfig is the configuration specific for txn kv restore tasks.
type RestoreTxnConfig struct {
	Config
	RestoreCommonConfig

	NoSchema           bool          `json:"no-schema" toml:"no-schema"`
	PDConcurrency      uint          `json:"pd-concurrency" toml:"pd-concurrency"`
	BatchFlushInterval time.Duration `json:"batch-flush-interval" toml:"batch-flush-interval"`
}

// ParseFromFlags parses the restore-related flags from the flag set.
func (cfg *RestoreTxnConfig) ParseFromFlags(flags *pflag.FlagSet) error {
	var err error
	cfg.NoSchema, err = flags.GetBool(flagNoSchema)
	if err != nil {
		return errors.Trace(err)
	}
	err = cfg.Config.ParseFromFlags(flags)
	if err != nil {
		return errors.Trace(err)
	}
	err = cfg.RestoreCommonConfig.ParseFromFlags(flags)
	if err != nil {
		return errors.Trace(err)
	}

	if cfg.Config.Concurrency == 0 {
		cfg.Config.Concurrency = defaultRestoreConcurrency
	}
	cfg.PDConcurrency, err = flags.GetUint(FlagPDConcurrency)
	if err != nil {
		return errors.Annotatef(err, "failed to get flag %s", FlagPDConcurrency)
	}
	cfg.BatchFlushInterval, err = flags.GetDuration(FlagBatchFlushInterval)
	if err != nil {
		return errors.Annotatef(err, "failed to get flag %s", FlagBatchFlushInterval)
	}
	return nil
}

// adjustRestoreConfig is use for BR(binary) and BR in TiDB.
// When new config was add and not included in parser.
// we should set proper value in this function.
// so that both binary and TiDB will use same default value.
func (cfg *RestoreTxnConfig) adjustRestoreConfig() {
	cfg.Config.adjust()
	cfg.RestoreCommonConfig.adjust()

	if cfg.Config.Concurrency == 0 {
		cfg.Config.Concurrency = defaultRestoreConcurrency
	}
	if cfg.Config.SwitchModeInterval == 0 {
		cfg.Config.SwitchModeInterval = defaultSwitchInterval
	}
	if cfg.PDConcurrency == 0 {
		cfg.PDConcurrency = defaultPDConcurrency
	}
	if cfg.BatchFlushInterval == 0 {
		cfg.BatchFlushInterval = defaultBatchFlushInterval
	}
}

// DefineTxnRestoreFlags defines the required flags for `lavadb` subcommand.
func DefineTxnRestoreFlags(command *cobra.Command) {
	command.Flags().StringP(flagKeyFormat, "", "hex", "start/end key format, support raw|escaped|hex")
	command.Flags().StringP(flagStartKey, "", "", "restore raw kv start key, key is inclusive")
	command.Flags().StringP(flagEndKey, "", "", "restore raw kv end key, key is exclusive")
}

// RunRestoreTxn starts a restore task inside the current goroutine.
func RunRestoreTxn(c context.Context, g glue.Glue, cmdName string, cfg *RestoreTxnConfig) error {
	cfg.adjustRestoreConfig()

	defer summary.Summary(cmdName)
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("task.RunRestoreTxn", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	needDomain := false
	mgr, err := NewMgr(ctx, g, cfg.PD, cfg.TLS, GetKeepalive(&cfg.Config), cfg.CheckRequirements, needDomain)
	if err != nil {
		return errors.Trace(err)
	}
	defer mgr.Close()

	keepaliveCfg := GetKeepalive(&cfg.Config)
	// sometimes we have pooled the connections.
	// sending heartbeats in idle times is useful.
	keepaliveCfg.PermitWithoutStream = true
	client, err := restore.NewRestoreClient(g, mgr.GetPDClient(), mgr.GetStorage(), mgr.GetTLSConfig(), keepaliveCfg)
	if err != nil {
		return errors.Trace(err)
	}
	defer client.Close()

	client.SetRateLimit(cfg.RateLimit)
	client.SetCrypter(&cfg.CipherInfo)
	client.SetConcurrency(uint(cfg.Concurrency))
	if cfg.Online {
		client.EnableOnline()
	}

	client.SetSwitchModeInterval(cfg.SwitchModeInterval)

	u, s, backupMeta, err := ReadBackupMeta(ctx, metautil.MetaFile, &cfg.Config)
	if err != nil {
		return errors.Trace(err)
	}
	backupVersion := version.NormalizeBackupVersion(backupMeta.ClusterVersion)
	if cfg.CheckRequirements && backupVersion != nil {
		if versionErr := version.CheckClusterVersion(ctx, mgr.GetPDClient(), version.CheckVersionForBackup(backupVersion)); versionErr != nil {
			return errors.Trace(versionErr)
		}
	}
	reader := metautil.NewMetaReader(backupMeta, s, &cfg.CipherInfo)
	if err = client.InitBackupMetaForTxn(c, backupMeta, u, s, reader); err != nil {
		return errors.Trace(err)
	}

	if client.IsRawKvMode() {
		return errors.Annotate(berrors.ErrRestoreModeMismatch, "cannot do transactional restore from raw kv data")
	}

	files := backupMeta.Files
	archiveSize := reader.ArchiveSize(ctx, files)
	g.Record(summary.RestoreDataSize, archiveSize)
	g.Record("Size", archiveSize)
	restoreTS, err := client.GetTS(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	sp := utils.BRServiceSafePoint{
		BackupTS: restoreTS,
		TTL:      utils.DefaultBRGCSafePointTTL,
		ID:       utils.MakeSafePointID(),
	}
	g.Record("BackupTS", restoreTS)

	// restore checksum will check safe point with its start ts, see details at
	// https://github.com/pingcap/tidb/blob/180c02127105bed73712050594da6ead4d70a85f/store/tikv/kv.go#L186-L190
	// so, we should keep the safe point unchangeable. to avoid GC life time is shorter than transaction duration.
	err = utils.StartServiceSafePointKeeper(ctx, mgr.GetPDClient(), sp)
	if err != nil {
		return errors.Trace(err)
	}

	if len(files) == 0 {
		log.Info("no files, empty databases and tables are restored")
		summary.SetSuccessStatus(true)
		return nil
	}

	summary.CollectInt("restore files", len(files))

	ranges, _, err := restore.MergeFileRanges(
		files, cfg.MergeSmallRegionKeyCount, cfg.MergeSmallRegionKeyCount)
	if err != nil {
		return errors.Trace(err)
	}

	// Redirect to log if there is no log file to avoid unreadable output.
	// TODO: How to show progress?
	updateCh := g.StartProgress(
		ctx,
		cmdName,
		// Split/Scatter + Download/Ingest
		int64(len(ranges)+len(files)),
		!cfg.LogProgress)

	// txn restore does not need to rewrite keys.
	rewrite := &restore.RewriteRules{}
	err = restore.SplitRanges(ctx, client, ranges, rewrite, updateCh)
	if err != nil {
		return errors.Trace(err)
	}

	restoreSchedulers, err := restorePreWork(ctx, client, mgr)
	if err != nil {
		return errors.Trace(err)
	}
	// Always run the post-work even on error, so we don't stuck in the import
	// mode or emptied schedulers
	defer restorePostWork(ctx, client, restoreSchedulers)

	err = client.RestoreFiles(ctx, files, &restore.RewriteRules{
		Data: []*import_sstpb.RewriteRule{{
			OldKeyPrefix: []byte{},
			NewKeyPrefix: []byte{},
			NewTimestamp: 0,
		}},
	}, updateCh)
	//err = client.RestoreRaw(ctx, cfg.StartKey, cfg.EndKey, files, updateCh)
	if err != nil {
		return errors.Trace(err)
	}

	// Restore has finished.
	updateCh.Close()

	// Set task summary to success status.
	summary.SetSuccessStatus(true)
	return nil
}
