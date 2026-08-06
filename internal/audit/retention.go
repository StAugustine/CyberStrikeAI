package audit

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// auditRetentionPurgeInterval is how often PurgeExpired runs while the process is up (startup also purges once).
const auditRetentionPurgeInterval = time.Hour

// StartRetentionLoop periodically purges expired audit rows until ctx is cancelled.
func StartRetentionLoop(ctx context.Context, s *Service, logger *zap.Logger) <-chan struct{} {
	done := make(chan struct{})
	if s == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(auditRetentionPurgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.PurgeExpired()
				if logger != nil {
					logger.Debug("audit retention tick completed")
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return done
}
