package cliproxy

import (
	"context"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// startQuotaHistory loads persisted quota history, seeds the auth manager with
// the last known observations so quota-aware routing survives restarts, and
// records every new observation.
func (s *Service) startQuotaHistory(ctx context.Context, cfg *config.Config) {
	if s == nil || s.coreManager == nil {
		return
	}
	path := ""
	authDir, errResolve := resolveCooldownStateAuthDir(cfg)
	if errResolve != nil {
		log.Warnf("quota history: failed to resolve auth directory, keeping history in memory: %v", errResolve)
	} else if authDir != "" {
		path = filepath.Join(authDir, quotahistory.FileName)
	}

	store := quotahistory.New(path)
	if errLoad := store.Load(); errLoad != nil {
		log.Warnf("quota history: %v", errLoad)
	}
	if restored := store.RestoreInto(s.coreManager); restored > 0 {
		log.Infof("quota history: restored quota observations for %d credential(s)", restored)
	}
	s.coreManager.SetQuotaObservationHook(store.Record)
	s.quotaHistory = store
	quotahistory.SetDefault(store)
	go store.Run(ctx)
}

func (s *Service) flushQuotaHistory() {
	if s == nil || s.quotaHistory == nil {
		return
	}
	if errFlush := s.quotaHistory.Flush(); errFlush != nil {
		log.Warnf("quota history: flush on shutdown failed: %v", errFlush)
	}
}
