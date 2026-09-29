package server

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/casosorg/casos/conf"
	"github.com/casosorg/casos/util"
	"github.com/sirupsen/logrus"
	"go.etcd.io/etcd/server/v3/embed"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// A kine query that runs for hours keeps the SQLite WAL from ever restarting,
// so the WAL grows without bound and every read gets slower.
const (
	defaultKineQueryTimeout       = time.Minute
	defaultKineCheckpointInterval = time.Minute
	kineWalTruncateSize           = 64 << 20

	etcdRangeMethod       = "/etcdserverpb.KV/Range"
	kineGRPCOverheadBytes = 512 * 1024
)

func kineDurationConfig(key string, defaultValue time.Duration) time.Duration {
	raw := strings.TrimSpace(conf.GetConfigString(key))
	if raw == "" {
		return defaultValue
	}
	if raw == "0" {
		return 0
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		logrus.Warnf("invalid %s %q, using %s", key, raw, defaultValue)
		return defaultValue
	}
	return value
}

// newKineGRPCServer has the options of kine's default server plus a deadline on every Range.
func newKineGRPCServer(queryTimeout time.Duration) *grpc.Server {
	options := []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             embed.DefaultGRPCKeepAliveMinTime,
			PermitWithoutStream: false,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    embed.DefaultGRPCKeepAliveInterval,
			Timeout: embed.DefaultGRPCKeepAliveTimeout,
		}),
		grpc.MaxConcurrentStreams(embed.DefaultMaxConcurrentStreams),
		grpc.MaxRecvMsgSize(int(embed.DefaultMaxRequestBytes) + kineGRPCOverheadBytes),
		grpc.MaxSendMsgSize(math.MaxInt32),
	}
	if queryTimeout > 0 {
		options = append(options, grpc.UnaryInterceptor(rangeTimeoutInterceptor(queryTimeout)))
	}
	return grpc.NewServer(options...)
}

func rangeTimeoutInterceptor(timeout time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod != etcdRangeMethod {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, req)
	}
}

// startKineWalCheckpointer truncates a large WAL without waiting for readers or writers
// (busy timeout 0), so a checkpoint that cannot finish is simply retried on the next tick.
func startKineWalCheckpointer(ctx context.Context, datastoreEndpoint string, interval time.Duration) error {
	if interval <= 0 || !strings.HasPrefix(datastoreEndpoint, "sqlite://") {
		return nil
	}
	databasePath, err := util.SQLiteDatabasePath(strings.TrimPrefix(datastoreEndpoint, "sqlite://"))
	if err != nil {
		return err
	}
	if databasePath == "" {
		return nil
	}
	databasePath, err = filepath.Abs(databasePath)
	if err != nil {
		return err
	}

	uriPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", "file://"+uriPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		return fmt.Errorf("open kine database for WAL checkpoints: %w", err)
	}
	db.SetMaxOpenConns(1)

	walPath := databasePath + "-wal"
	go func() {
		defer db.Close()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkpointKineWal(ctx, db, walPath)
			}
		}
	}()
	return nil
}

func checkpointKineWal(ctx context.Context, db *sql.DB, walPath string) {
	info, err := os.Stat(walPath)
	if err != nil || info.Size() < kineWalTruncateSize {
		return
	}

	start := time.Now()
	var busy, logFrames, checkpointed int
	err = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		logrus.Warnf("kine WAL checkpoint: %v", err)
		return
	}
	if busy != 0 {
		logrus.Warnf("kine WAL is %d MB and still in use, checkpointed %d of %d frames", info.Size()>>20, checkpointed, logFrames)
		return
	}
	logrus.Infof("kine WAL truncated from %d MB in %s", info.Size()>>20, time.Since(start).Round(time.Millisecond))
}
