package account

import (
	"context"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// sweepPreparations cancels the account preparations that expired, were
// abandoned or are being cancelled (core.account.preparation_cleanup), and
// returns how many it disposed of.
func (s *Service) sweepPreparations(ctx context.Context) (int, error) {
	var expired []model.AccountPreparation
	if err := store.G(ctx, s.d.DB.Pool).Select(preparationColumns).
		Where("account_id IS NULL AND state<>'cancelled' AND (state='cancelling' OR expires_at<=now() OR last_seen_at<now()-interval '15 minutes')").
		Order("created_at").Limit(20).Find(&expired).Error; err != nil {
		return 0, err
	}
	for i, p := range expired {
		call, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := s.disposePreparation(call, preparationOf(p), disposeExpired)
		cancel()
		if err != nil {
			return i, err
		}
	}
	return len(expired), nil
}
