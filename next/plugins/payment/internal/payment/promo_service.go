package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/plugins/payment/internal/model"
)

// PromoService handles promotional code operations
type PromoService struct {
	db     dbFunc
	credit creditFunc
}

// NewPromoService creates a new PromoService
func NewPromoService(db dbFunc, credit creditFunc) *PromoService {
	return &PromoService{db: db, credit: credit}
}

// CreatePromoCodeRequest represents promo code creation request
type CreatePromoCodeRequest struct {
	Code        string          `json:"code,omitempty"`
	BonusAmount decimal.Decimal `json:"bonus_amount"`
	MaxUses     int             `json:"max_uses,omitempty"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	Notes       string          `json:"notes,omitempty"`
}

// CreatePromoCode creates a new promotional code
func (s *PromoService) CreatePromoCode(ctx context.Context, req CreatePromoCodeRequest) (*PromoCode, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	code := strings.TrimSpace(strings.ToUpper(req.Code))
	if code == "" {
		// Generate random code
		rc, err := GenerateRedeemCode()
		if err != nil {
			return nil, fmt.Errorf("generate code: %w", err)
		}
		code = rc[:16] // Use first 16 chars
	}

	if req.BonusAmount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("bonus amount must be positive")
	}

	// RETURNING * reads the row back as stored (the amount at the column's
	// scale). The zero MaxUses GORM replaces with the column default is 0 too.
	row := model.PromoCode{Code: code, BonusAmount: req.BonusAmount, MaxUses: req.MaxUses,
		Status: PromoStatusActive, ExpiresAt: req.ExpiresAt, Notes: &req.Notes}
	if err := db.Clauses(clause.Returning{}).Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, fmt.Errorf("code already exists")
		}
		return nil, fmt.Errorf("insert promo code: %w", err)
	}
	pc := promoCodeOf(row)

	slog.Info("promo code created", "id", pc.ID, "code", pc.Code, "bonus", pc.BonusAmount.String())
	return &pc, nil
}

// ApplyPromoCode applies a promo code for a user
func (s *PromoService) ApplyPromoCode(ctx context.Context, userID int64, code string) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	code = strings.TrimSpace(strings.ToUpper(code))
	if code == "" {
		return fmt.Errorf("code is required")
	}

	var pc PromoCode
	var ledgerID int64
	var duplicate bool
	err = withLockedRow(db, lockedRow{
		columns:   "id, code, bonus_amount, max_uses, used_count, status, expires_at",
		where:     "code = ?",
		arg:       code,
		missing:   "invalid promo code",
		queryFail: "query promo code",
	}, func(tx *gorm.DB, row model.PromoCode) error {
		pc = promoCodeOf(row)
		// Validate promo code
		if !pc.CanUse() {
			if pc.Status != PromoStatusActive {
				return fmt.Errorf("promo code is disabled")
			}
			if pc.ExpiresAt != nil && time.Now().After(*pc.ExpiresAt) {
				return fmt.Errorf("promo code expired")
			}
			if pc.MaxUses > 0 && pc.UsedCount >= pc.MaxUses {
				return fmt.Errorf("promo code usage limit reached")
			}
			return fmt.Errorf("promo code not available")
		}

		// Check if user already used this code
		var existingUsage int64
		if err := tx.Model(&model.PromoCodeUsage{}).Where("promo_code_id = ? AND user_id = ?", pc.ID, userID).
			Count(&existingUsage).Error; err != nil {
			return fmt.Errorf("check existing usage: %w", err)
		}
		if existingUsage > 0 {
			return fmt.Errorf("you have already used this promo code")
		}

		// Credit balance
		resp, err := s.credit(ctx, pluginsdk.LedgerChange{
			UserID:         userID,
			Amount:         pc.BonusAmount.String(),
			RefType:        "promo_code",
			RefID:          strconv.FormatInt(pc.ID, 10),
			IdempotencyKey: fmt.Sprintf("promo_%d_%d", pc.ID, userID),
			Note:           fmt.Sprintf("优惠码 %s 赠送 ¥%s", pc.Code, pc.BonusAmount.String()),
		})
		if err != nil {
			return fmt.Errorf("credit ledger: %w", err)
		}
		ledgerID, duplicate = resp.LedgerID, resp.Duplicate

		// Record usage
		now := time.Now()
		if err := tx.Create(&model.PromoCodeUsage{
			PromoCodeID: pc.ID, UserID: userID, BonusAmount: pc.BonusAmount, UsedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("insert usage record: %w", err)
		}

		// Increment used count
		if err := promoCodes(tx).Where("id = ?", pc.ID).Updates(map[string]any{
			"used_count": gorm.Expr("used_count + 1"), "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("increment used count: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	slog.Info("promo code applied",
		"promo_id", pc.ID,
		"code", pc.Code,
		"user_id", userID,
		"bonus", pc.BonusAmount.String(),
		"ledger_id", ledgerID,
		"duplicate", duplicate)
	return nil
}

// ListPromoCodes lists all promo codes (admin only)
func (s *PromoService) ListPromoCodes(ctx context.Context, status string, limit, offset int) ([]PromoCode, int, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, 0, err
	}
	// The status filter applies to the count and the page alike (listPaged).
	q := pagedQuery{
		columns:    selectPromoCodes,
		orderBy:    "created_at DESC",
		countLabel: "count promo codes",
		queryLabel: "query promo codes",
	}
	if status != "" {
		q.where, q.args = "status = ?", []any{status}
	}
	return listPaged(db, q, limit, offset, promoCodeOf)
}

// selectPromoCodes is the column list shared by the admin list statements.
const selectPromoCodes = `id, code, bonus_amount, max_uses, used_count, status,
	expires_at, notes, created_at`

// promoCodes selects from promo_codes.
func promoCodes(db *gorm.DB) *gorm.DB { return db.Model(&model.PromoCode{}) }

// promoCodeOf is the promo code of a row; NULL notes read as "".
func promoCodeOf(r model.PromoCode) PromoCode {
	return PromoCode{
		ID:          r.ID,
		Code:        r.Code,
		BonusAmount: r.BonusAmount,
		MaxUses:     r.MaxUses,
		UsedCount:   r.UsedCount,
		Status:      r.Status,
		ExpiresAt:   r.ExpiresAt,
		Notes:       deref(r.Notes),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// UpdatePromoCode updates a promo code
func (s *PromoService) UpdatePromoCode(ctx context.Context, id int64, status string, maxUses int) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	result := promoCodes(db).Where("id = ?", id).Updates(map[string]any{
		"status": status, "max_uses": maxUses, "updated_at": now,
	})
	if err := result.Error; err != nil {
		return fmt.Errorf("update promo code: %w", err)
	}

	if result.RowsAffected == 0 {
		return fmt.Errorf("promo code not found")
	}

	slog.Info("promo code updated", "id", id, "status", status, "max_uses", maxUses)
	return nil
}

// DeletePromoCode deletes a promo code
func (s *PromoService) DeletePromoCode(ctx context.Context, id int64) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	result := db.Delete(&model.PromoCode{}, id)
	if err := result.Error; err != nil {
		return fmt.Errorf("delete promo code: %w", err)
	}

	if result.RowsAffected == 0 {
		return fmt.Errorf("promo code not found")
	}

	slog.Info("promo code deleted", "id", id)
	return nil
}
