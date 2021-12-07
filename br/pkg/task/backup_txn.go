package task

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/backup"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/rtree"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
)

// TxnBackupConfig is the configuration specific for txn kv range backup tasks.
type TxnBackupConfig struct {
	Config

	TimeAgo          time.Duration `json:"time-ago" toml:"time-ago"`
	BackupTS         uint64        `json:"backup-ts" toml:"backup-ts"`
	LastBackupTS     uint64        `json:"last-backup-ts" toml:"last-backup-ts"`
	GCTTL            int64         `json:"gc-ttl" toml:"gc-ttl"`
	RemoveSchedulers bool          `json:"remove-schedulers" toml:"remove-schedulers"`
	IgnoreStats      bool          `json:"ignore-stats" toml:"ignore-stats"`
	UseBackupMetaV2  bool          `json:"use-backupmeta-v2"`
	StartKey         []byte        `json:"start-key" toml:"start-key"`
	EndKey           []byte        `json:"end-key" toml:"end-key"`
	CompressionConfig
}

// DefineTxnBackupFlags defines common flags for the backup command.
func DefineTxnBackupFlags(command *cobra.Command) {
	command.Flags().StringP(flagKeyFormat, "", "hex", "start/end key format, support raw|escaped|hex")
	command.Flags().StringP(flagStartKey, "", "", "restore raw kv start key, key is inclusive")
	command.Flags().StringP(flagEndKey, "", "", "restore raw kv end key, key is exclusive")

	command.Flags().String(flagCompressionType, "zstd",
		"backup sst file compression algorithm, value can be one of 'lz4|zstd|snappy'")
	command.Flags().Bool(flagRemoveSchedulers, false,
		"disable the balance, shuffle and region-merge schedulers in PD to speed up backup")
	// This flag can impact the online cluster, so hide it in case of abuse.
	_ = command.Flags().MarkHidden(flagRemoveSchedulers)
}

// ParseTxnBackupConfigFromFlags parses the backup-related flags from the flag set.
func (cfg *TxnBackupConfig) ParseTxnBackupConfigFromFlags(flags *pflag.FlagSet) error {
	timeAgo, err := flags.GetDuration(flagBackupTimeago)
	if err != nil {
		return errors.Trace(err)
	}
	if timeAgo < 0 {
		return errors.Annotate(berrors.ErrInvalidArgument, "negative timeago is not allowed")
	}
	cfg.TimeAgo = timeAgo
	cfg.LastBackupTS, err = flags.GetUint64(flagLastBackupTS)
	if err != nil {
		return errors.Trace(err)
	}
	backupTS, err := flags.GetString(flagBackupTS)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.BackupTS, err = parseTSString(backupTS)
	if err != nil {
		return errors.Trace(err)
	}
	gcTTL, err := flags.GetInt64(flagGCTTL)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.GCTTL = gcTTL

	compressionCfg, err := parseCompressionFlags(flags)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.CompressionConfig = *compressionCfg

	if err = cfg.Config.ParseFromFlags(flags); err != nil {
		return errors.Trace(err)
	}
	cfg.RemoveSchedulers, err = flags.GetBool(flagRemoveSchedulers)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.IgnoreStats, err = flags.GetBool(flagIgnoreStats)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.UseBackupMetaV2, err = flags.GetBool(flagUseBackupMetaV2)
	if err != nil {
		return errors.Trace(err)
	}

	format, err := flags.GetString(flagKeyFormat)
	if err != nil {
		return errors.Trace(err)
	}
	start, err := flags.GetString(flagStartKey)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.StartKey, err = utils.ParseKey(format, start)
	if err != nil {
		return errors.Trace(err)
	}
	end, err := flags.GetString(flagEndKey)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.EndKey, err = utils.ParseKey(format, end)
	if err != nil {
		return errors.Trace(err)
	}
	if len(cfg.StartKey) > 0 && len(cfg.EndKey) > 0 && bytes.Compare(cfg.StartKey, cfg.EndKey) >= 0 {
		return errors.Annotate(berrors.ErrBackupInvalidRange, "endKey must be greater than startKey")
	}
	return nil
}

// adjustBackupConfig is use for BR(binary) and BR in TiDB.
// When new config was add and not included in parser.
// we should set proper value in this function.
// so that both binary and TiDB will use same default value.
func (cfg *TxnBackupConfig) adjustBackupConfig() {
	cfg.adjust()
	usingDefaultConcurrency := false
	if cfg.Config.Concurrency == 0 {
		cfg.Config.Concurrency = defaultBackupConcurrency
		usingDefaultConcurrency = true
	}
	if cfg.Config.Concurrency > maxBackupConcurrency {
		cfg.Config.Concurrency = maxBackupConcurrency
	}
	if cfg.RateLimit != unlimited {
		// TiKV limits the upload rate by each backup request.
		// When the backup requests are sent concurrently,
		// the ratelimit couldn't work as intended.
		// Degenerating to sequentially sending backup requests to avoid this.
		if !usingDefaultConcurrency {
			logutil.WarnTerm("setting `--ratelimit` and `--concurrency` at the same time, "+
				"ignoring `--concurrency`: `--ratelimit` forces sequential (i.e. concurrency = 1) backup",
				zap.String("ratelimit", units.HumanSize(float64(cfg.RateLimit))+"/s"),
				zap.Uint32("concurrency-specified", cfg.Config.Concurrency))
		}
		cfg.Config.Concurrency = 1
	}

	if cfg.GCTTL == 0 {
		cfg.GCTTL = utils.DefaultBRGCSafePointTTL
	}
	// Use zstd as default
	if cfg.CompressionType == backuppb.CompressionType_UNKNOWN {
		cfg.CompressionType = backuppb.CompressionType_ZSTD
	}
}

func buildTxnBackupRange(tid int32) []rtree.Range {
	// https://git.woa.com/cos-backend/lavadb-adapter/blob/master/pkg/encoding/encoding.go
	var startKey [5]byte
	var endKey [5]byte
	binary.LittleEndian.PutUint32(startKey[0:4], uint32(tid))
	binary.LittleEndian.PutUint32(endKey[0:4], uint32(tid))
	startKey[4] = 0
	endKey[4] = 255

	return []rtree.Range{
		{
			StartKey: startKey[:],
			EndKey:   endKey[:],
			Files:    nil,
		},
	}
}

// RunBackupLavadb starts a backup lavadb tables task inside the current goroutine.
func RunBackupLavadb(c context.Context, g glue.Glue, cmdName string, cfg *TxnBackupConfig) error {
	cfg.adjustBackupConfig()

	defer summary.Summary(cmdName)
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("task.RunBackupTxn", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	u, err := storage.ParseBackend(cfg.Storage, &cfg.BackendOptions)
	if err != nil {
		return errors.Trace(err)
	}
	skipStats := cfg.IgnoreStats
	// For backup, Domain is not needed if user ignores stats.
	// Domain loads all table info into memory. By skipping Domain, we save
	// lots of memory (about 500MB for 40K 40 fields YCSB tables).
	needDomain := !skipStats
	mgr, err := NewMgr(ctx, g, cfg.PD, cfg.TLS, GetKeepalive(&cfg.Config), cfg.CheckRequirements, needDomain)
	if err != nil {
		return errors.Trace(err)
	}
	defer mgr.Close()

	client, err := backup.NewBackupClient(ctx, mgr)
	if err != nil {
		return errors.Trace(err)
	}
	opts := storage.ExternalStorageOptions{
		NoCredentials:   cfg.NoCreds,
		SendCredentials: cfg.SendCreds,
		SkipCheckPath:   cfg.SkipCheckPath,
	}
	if err = client.SetStorage(ctx, u, &opts); err != nil {
		return errors.Trace(err)
	}
	err = client.SetLockFile(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	client.SetGCTTL(cfg.GCTTL)

	backupTS, err := client.GetTS(ctx, cfg.TimeAgo, cfg.BackupTS)
	if err != nil {
		return errors.Trace(err)
	}
	g.Record("BackupTS", backupTS)
	sp := utils.BRServiceSafePoint{
		BackupTS: backupTS,
		TTL:      client.GetGCTTL(),
		ID:       utils.MakeSafePointID(),
	}
	// use lastBackupTS as safePoint if exists
	if cfg.LastBackupTS > 0 {
		sp.BackupTS = cfg.LastBackupTS
	}

	log.Info("current backup safePoint job", zap.Object("safePoint", sp))
	err = utils.StartServiceSafePointKeeper(ctx, mgr.GetPDClient(), sp)
	if err != nil {
		return errors.Trace(err)
	}

	isIncrementalBackup := cfg.LastBackupTS > 0

	if cfg.RemoveSchedulers {
		log.Debug("removing some PD schedulers")
		restore, e := mgr.RemoveSchedulers(ctx)
		defer func() {
			if ctx.Err() != nil {
				log.Warn("context canceled, doing clean work with background context")
				ctx = context.Background()
			}
			if restoreE := restore(ctx); restoreE != nil {
				log.Warn("failed to restore removed schedulers, you may need to restore them manually", zap.Error(restoreE))
			}
		}()
		if e != nil {
			return errors.Trace(err)
		}
	}

	req := backuppb.BackupRequest{
		ClusterId:        client.GetClusterID(),
		StartVersion:     cfg.LastBackupTS,
		EndVersion:       backupTS,
		RateLimit:        cfg.RateLimit,
		Concurrency:      defaultBackupConcurrency,
		CompressionType:  cfg.CompressionType,
		CompressionLevel: cfg.CompressionLevel,
		CipherInfo:       &cfg.CipherInfo,
	}
	brVersion := g.GetVersion()
	clusterVersion, err := mgr.GetClusterVersion(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	ranges := []rtree.Range{
		{
			StartKey: cfg.StartKey,
			EndKey:   cfg.EndKey,
			Files:    nil,
		},
	}

	// Metafile size should be less than 64MB.
	metawriter := metautil.NewMetaWriter(client.GetStorage(),
		metautil.MetaFileSize, cfg.UseBackupMetaV2, &cfg.CipherInfo)
	// Hack way to update backupmeta.
	metawriter.Update(func(m *backuppb.BackupMeta) {
		m.StartVersion = req.StartVersion
		m.EndVersion = req.EndVersion
		m.IsRawKv = req.IsRawKv
		m.ClusterId = req.ClusterId
		m.ClusterVersion = clusterVersion
		m.BrVersion = brVersion
	})

	// nothing to backup
	if ranges == nil {
		pdAddress := strings.Join(cfg.PD, ",")
		log.Warn("Nothing to backup, maybe connected to cluster for restoring",
			zap.String("PD address", pdAddress))

		err = metawriter.FlushBackupMeta(ctx)
		if err == nil {
			summary.SetSuccessStatus(true)
		}
		return err
	}

	if isIncrementalBackup {
		if backupTS <= cfg.LastBackupTS {
			log.Error("LastBackupTS is larger or equal to current TS")
			return errors.Annotate(berrors.ErrInvalidArgument, "LastBackupTS is larger or equal to current TS")
		}
		err = utils.CheckGCSafePoint(ctx, mgr.GetPDClient(), cfg.LastBackupTS)
		if err != nil {
			log.Error("Check gc safepoint for last backup ts failed", zap.Error(err))
			return errors.Trace(err)
		}

		metawriter.StartWriteMetasAsync(ctx, metautil.AppendDDL)
		err = backup.WriteBackupDDLJobs(metawriter, mgr.GetStorage(), cfg.LastBackupTS, backupTS)
		if err != nil {
			return errors.Trace(err)
		}
		if err = metawriter.FinishWriteMetas(ctx, metautil.AppendDDL); err != nil {
			return errors.Trace(err)
		}
	}

	summary.CollectInt("backup total ranges", len(ranges))

	var updateCh glue.Progress
	var unit backup.ProgressUnit
	if len(ranges) < 100 {
		unit = backup.RegionUnit
		// The number of regions need to backup
		approximateRegions := 0
		for _, r := range ranges {
			var regionCount int
			regionCount, err = mgr.GetRegionCount(ctx, r.StartKey, r.EndKey)
			if err != nil {
				return errors.Trace(err)
			}
			approximateRegions += regionCount
		}
		// Redirect to log if there is no log file to avoid unreadable output.
		updateCh = g.StartProgress(
			ctx, cmdName, int64(approximateRegions), !cfg.LogProgress)
		summary.CollectInt("backup total regions", approximateRegions)
	} else {
		unit = backup.RangeUnit
		// To reduce the costs, we can use the range as unit of progress.
		updateCh = g.StartProgress(
			ctx, cmdName, int64(len(ranges)), !cfg.LogProgress)
	}

	progressCount := 0
	progressCallBack := func(callBackUnit backup.ProgressUnit) {
		if unit == callBackUnit {
			updateCh.Inc()
			progressCount++
			failpoint.Inject("progress-call-back", func(v failpoint.Value) {
				log.Info("failpoint progress-call-back injected")
				if fileName, ok := v.(string); ok {
					f, osErr := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY, os.ModePerm)
					if osErr != nil {
						log.Warn("failed to create file", zap.Error(osErr))
					}
					msg := []byte(fmt.Sprintf("%s:%d\n", unit, progressCount))
					_, err = f.Write(msg)
					if err != nil {
						log.Warn("failed to write data to file", zap.Error(err))
					}
				}
			})
		}
	}
	metawriter.StartWriteMetasAsync(ctx, metautil.AppendDataFile)
	err = client.BackupRanges(ctx, ranges, req, uint(cfg.Concurrency), metawriter, progressCallBack)
	if err != nil {
		return errors.Trace(err)
	}
	// Backup has finished
	updateCh.Close()

	err = metawriter.FinishWriteMetas(ctx, metautil.AppendDataFile)
	if err != nil {
		return errors.Trace(err)
	}

	err = metawriter.FlushBackupMeta(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	archiveSize := metawriter.ArchiveSize()
	g.Record(summary.BackupDataSize, archiveSize)
	//backup from tidb will fetch a general Size issue https://github.com/pingcap/tidb/issues/27247
	g.Record("Size", archiveSize)
	failpoint.Inject("s3-outage-during-writing-file", func(v failpoint.Value) {
		log.Info("failpoint s3-outage-during-writing-file injected, " +
			"process will sleep for 3s and notify the shell to kill s3 service.")
		if sigFile, ok := v.(string); ok {
			file, err := os.Create(sigFile)
			if err != nil {
				log.Warn("failed to create file for notifying, skipping notify", zap.Error(err))
			}
			if file != nil {
				file.Close()
			}
		}
		time.Sleep(3 * time.Second)
	})
	// Set task summary to success status.
	summary.SetSuccessStatus(true)
	return nil
}
