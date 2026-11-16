package payment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// RedeemService handles redeem code operations
type RedeemService struct {
	db     dbFunc
	credit creditFunc
}

// NewRedeemService creates a new RedeemService
func NewRedeemService(db dbFunc, credit creditFunc) *RedeemService {
	return &RedeemService{db: db, credit: credit}
}

// GenerateRedeemCode generates a random redeem code
func GenerateRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return strings.ToUpper(hex.EncodeToString(b)), nil
}

// GenerateCodesRequest represents batch code generation request
type GenerateCodesRequest struct {
	Count        int             `json:"count"`
	Type         string          `json:"type"`
	Value        decimal.Decimal `json:"value"`
	GroupID      *int64          `json:"group_id,omitempty"`
	ValidityDays int             `json:"validity_days,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	Notes        string          `json:"notes,omitempty"`
}

// GenerateCodes generates multiple redeem codes
func (s *RedeemService) GenerateCodes(ctx context.Context, req GenerateCodesRequest) ([]RedeemCode, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	if req.Count <= 0 || req.Count > 1000 {
		return nil, fmt.Errorf("invalid count: must be 1-1000")
	}

	if req.Value.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("invalid value: must be positive")
	}

	var codes []RedeemCode
	err = inTx(db, func(tx *gorm.DB) error {
		for i := 0; i < req.Count; i++ {
			code, err := GenerateRedeemCode()
			if err != nil {
				return fmt.Errorf("generate code: %w", err)
			}

			codeHash := HashRedeemCode(code)

			// A map rather than a model: GORM would store the column
			// default for an empty type ('balance'), this stores ''.
			row := map[string]any{"code": code, "code_hash": codeHash, "type": req.Type, "value": req.Value,
				"group_id": req.GroupID, "validity_days": req.ValidityDays, "status": RedeemStatusUnused,
				"expires_at": req.ExpiresAt, "notes": req.Notes}
			err = tx.Model(&model.RedeemCode{}).
				Clauses(clause.Returning{Columns: []clause.Column{{Name: "id"}, {Name: "created_at"}}}).Create(row).Error
			if err != nil {
				return fmt.Errorf("insert code: %w", err)
			}

			rc := RedeemCode{
				ID:           row["id"].(int64),
				Code:         code,
				CodeHash:     codeHash,
				Type:         req.Type,
				Value:        req.Value,
				GroupID:      req.GroupID,
				ValidityDays: req.ValidityDays,
				Status:       RedeemStatusUnused,
				ExpiresAt:    req.ExpiresAt,
				Notes:        req.Notes,
				CreatedAt:    row["created_at"].(time.Time),
			}
			codes = append(codes, rc)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("redeem codes generated", "count", len(codes), "type", req.Type)
	return codes, nil
}

// RedeemCode redeems a code for a user
func (s *RedeemService) RedeemCode(ctx context.Context, userID int64, code string) (*RedeemCode, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(strings.ToUpper(code))
	if code == "" {
		return nil, fmt.Errorf("code is required")
	}

	var rc RedeemCode
	var now time.Time
	err = withLockedRow(db, lockedRow{
		columns:   selectRedeemCodes,
		where:     "code_hash = ?",
		arg:       HashRedeemCode(code),
		missing:   "invalid code",
		queryFail: "query code",
	}, func(tx *gorm.DB, row model.RedeemCode) error {
		rc = redeemCodeOf(row)
		// Validate code
		if !rc.CanUse() {
			if rc.IsUsed() {
				return fmt.Errorf("code already used")
			}
			if rc.IsExpired() {
				return fmt.Errorf("code expired")
			}
			return fmt.Errorf("code not available")
		}

		// Mark as used
		now = time.Now()
		if err := redeemCodes(tx).Where("id = ?", rc.ID).Updates(map[string]any{
			"status": RedeemStatusUsed, "used_by": userID, "used_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("update code: %w", err)
		}

		// Credit balance (only for balance type)
		if rc.Type != RedeemTypeBalance {
			return nil
		}
		resp, err := s.credit(ctx, pluginsdk.LedgerChange{
			UserID:         userID,
			Amount:         rc.Value.String(),
			RefType:        "redeem_code",
			RefID:          strconv.FormatInt(rc.ID, 10),
			IdempotencyKey: fmt.Sprintf("redeem_code_%d", rc.ID),
			Note:           fmt.Sprintf("兑换码充值 ¥%s", rc.Value.String()),
		})
		if err != nil {
			return fmt.Errorf("credit ledger: %w", err)
		}
		slog.Info("redeem code credited",
			"code_id", rc.ID,
			"user_id", userID,
			"amount", rc.Value.String(),
			"ledger_id", resp.LedgerID,
			"duplicate", resp.Duplicate)
		return nil
	})
	if err != nil {
		return nil, err
	}

	rc.Status = RedeemStatusUsed
	rc.UsedBy = &userID
	rc.UsedAt = &now

	slog.Info("redeem code redeemed", "code_id", rc.ID, "user_id", userID, "type", rc.Type)
	return &rc, nil
}

// ListCodes lists redeem codes (admin only)
func (s *RedeemService) ListCodes(ctx context.Context, status string, limit, offset int) ([]RedeemCode, int, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, 0, err
	}
	// The status filter applies to the count and the page alike (listPaged).
	q := pagedQuery{
		columns:    selectRedeemCodes,
		orderBy:    "created_at DESC",
		countLabel: "count codes",
		queryLabel: "query codes",
	}
	if status != "" {
		q.where, q.args = "status = ?", []any{status}
	}
	return listPaged(db, q, limit, offset, redeemCodeOf)
}

// selectRedeemCodes is the column list shared by the admin list statements
// and the redemption.
const selectRedeemCodes = `id, code_hash, type, value, group_id, validity_days,
	status, used_by, used_at, expires_at, notes, created_at`

// GetUserRedeemHistory returns user's redeem history
func (s *RedeemService) GetUserRedeemHistory(ctx context.Context, userID int64, limit, offset int) ([]RedeemCode, int, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, 0, err
	}
	return listPaged(db, pagedQuery{
		columns:    selectUserRedeemHistory,
		where:      "used_by = ?",
		args:       []any{userID},
		orderBy:    "used_at DESC",
		countLabel: "count user codes",
		queryLabel: "query user codes",
	}, limit, offset, func(row model.RedeemCode) RedeemCode {
		rc := redeemCodeOf(row)
		rc.UsedBy = &userID
		return rc
	})
}

// selectUserRedeemHistory is the column list of the user's own history: fewer
// columns than the admin list, and no group/validity/expiry detail.
const selectUserRedeemHistory = `id, code_hash, type, value, status, used_at, notes, created_at`

// redeemCodes selects from redeem_codes.
func redeemCodes(db *gorm.DB) *gorm.DB { return db.Model(&model.RedeemCode{}) }

// redeemCodeOf is the redeem code of a row; NULL validity and notes read as
// zero values.
func redeemCodeOf(r model.RedeemCode) RedeemCode {
	return RedeemCode{
		ID:           r.ID,
		Code:         r.Code,
		CodeHash:     r.CodeHash,
		Type:         r.Type,
		Value:        r.Value,
		GroupID:      r.GroupID,
		ValidityDays: deref(r.ValidityDays),
		Status:       r.Status,
		UsedBy:       r.UsedBy,
		UsedAt:       r.UsedAt,
		ExpiresAt:    r.ExpiresAt,
		Notes:        deref(r.Notes),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

// DeleteCode deletes (invalidates) a redeem code
func (s *RedeemService) DeleteCode(ctx context.Context, codeID int64) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	result := redeemCodes(db).Where("id = ? AND status = ?", codeID, RedeemStatusUnused).Updates(map[string]any{
		"status": RedeemStatusExpired, "updated_at": time.Now(),
	})
	if err := result.Error; err != nil {
		return fmt.Errorf("update code: %w", err)
	}

	if result.RowsAffected == 0 {
		return fmt.Errorf("code not found or already used")
	}

	slog.Info("redeem code invalidated", "code_id", codeID)
	return nil
}
