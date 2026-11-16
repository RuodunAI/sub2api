package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/plugins/payment/internal/model"
)

// OrderService handles payment order operations
type OrderService struct {
	db        dbFunc
	credit    creditFunc
	verifiers map[string]NotificationVerifier
}

// NewOrderService creates a new OrderService
func NewOrderService(db dbFunc, credit creditFunc) *OrderService {
	return &OrderService{db: db, credit: credit, verifiers: map[string]NotificationVerifier{}}
}

// CreateOrderRequest represents order creation request
type CreateOrderRequest struct {
	UserID      int64
	Amount      decimal.Decimal
	PaymentType string
	OrderType   string
	PlanID      *int64
	ClientIP    string
	SrcHost     string
	SrcURL      string
}

// CreateOrderResponse represents order creation response
type CreateOrderResponse struct {
	OrderID    int64
	OutTradeNo string
	PayURL     string
	QRCode     string
	ExpiresAt  time.Time
}

// CreateOrder creates a new payment order
func (s *OrderService) CreateOrder(ctx context.Context, req CreateOrderRequest) (*CreateOrderResponse, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	// Load payment config
	cfg, err := s.loadConfig(db)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if !cfg.Enabled {
		return nil, fmt.Errorf("payment system is disabled")
	}

	// Validate amount
	if req.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("invalid amount")
	}
	if cfg.MinAmount.GreaterThan(decimal.Zero) && req.Amount.LessThan(cfg.MinAmount) {
		return nil, fmt.Errorf("amount below minimum: %s", cfg.MinAmount.String())
	}
	if cfg.MaxAmount.GreaterThan(decimal.Zero) && req.Amount.GreaterThan(cfg.MaxAmount) {
		return nil, fmt.Errorf("amount exceeds maximum: %s", cfg.MaxAmount.String())
	}

	// Check pending orders limit
	if err := s.checkPendingLimit(db, req.UserID, cfg.MaxPendingOrders); err != nil {
		return nil, err
	}

	// Calculate pay amount (with fee)
	feeRate := cfg.RechargeFeeRate
	payAmount := req.Amount
	if feeRate.GreaterThan(decimal.Zero) {
		fee := req.Amount.Mul(feeRate).Div(decimal.NewFromInt(100)).Round(2)
		payAmount = req.Amount.Add(fee)
	}

	// Generate unique out_trade_no
	outTradeNo := OrderIDPrefix + uuid.New().String()

	// Calculate expiry
	timeout := time.Duration(cfg.OrderTimeoutMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	expiresAt := time.Now().Add(timeout)

	// TODO: Get user email from host (需要核心提供用户查询接口)
	userEmail := fmt.Sprintf("user_%d@example.com", req.UserID)

	// Create order in database. A map rather than a model: GORM would store
	// the column default for an empty order type ('balance'), this stores ''.
	var orderID int64
	err = inTx(db, func(tx *gorm.DB) error {
		row := map[string]any{"user_id": req.UserID, "user_email": userEmail, "out_trade_no": outTradeNo,
			"payment_type": req.PaymentType, "amount": req.Amount, "pay_amount": payAmount, "fee_rate": feeRate,
			"currency": CurrencyDefaultCNY, "order_type": req.OrderType, "status": StatusPending, "expires_at": expiresAt,
			"client_ip": req.ClientIP, "src_host": req.SrcHost, "src_url": req.SrcURL, "plan_id": req.PlanID}
		err := tx.Model(&model.Order{}).Clauses(clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(row).Error
		orderID, _ = row["id"].(int64)
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// TODO: Select provider instance and create payment
	// For MVP, return mock payment URL
	payURL := fmt.Sprintf("https://pay.example.com/checkout?order=%s", outTradeNo)

	slog.Info("payment order created",
		"order_id", orderID,
		"user_id", req.UserID,
		"amount", req.Amount.String(),
		"pay_amount", payAmount.String())

	return &CreateOrderResponse{
		OrderID:    orderID,
		OutTradeNo: outTradeNo,
		PayURL:     payURL,
		ExpiresAt:  expiresAt,
	}, nil
}

// HandlePaymentNotification applies one provider webhook call. It fails
// closed: the provider must have an enabled instance (ErrProviderNotConfigured)
// and a registered NotificationVerifier (ErrProviderNotImplemented), and one
// instance must accept the request (ErrNotificationRejected). Only a verified
// success notification reaches the order; every failure leaves it untouched.
func (s *OrderService) HandlePaymentNotification(ctx context.Context, providerKey string, req *pluginv1.HTTPRequest) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	notification, err := s.verifyNotification(ctx, db, providerKey, req)
	if err != nil {
		return err
	}
	slog.Info("payment notification verified", "provider", providerKey, "status", notification.Status)
	if notification.Status != NotificationStatusSuccess {
		return nil
	}

	// Find order
	var order model.Order
	err = db.Select("id, user_id, amount, pay_amount, status, currency").
		Where("out_trade_no = ?", notification.OutTradeNo).Take(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: %s", ErrOrderNotFound, notification.OutTradeNo)
	}
	if err != nil {
		return fmt.Errorf("query order: %w", err)
	}

	// Check if already processed
	if order.Status == StatusCompleted || order.Status == StatusRefunded {
		slog.Info("order already processed", "order_id", order.ID, "status", order.Status)
		return nil
	}

	// Verify amount
	tolerance := decimal.NewFromFloat(AmountToleranceCNY)
	diff := notification.Amount.Sub(order.PayAmount).Abs()
	if diff.GreaterThan(tolerance) {
		return fmt.Errorf("%w: expected %s, got %s", ErrAmountMismatch, order.PayAmount.String(), notification.Amount.String())
	}

	// Update order to paid
	now := time.Now()
	err = orders(db).Where("id = ? AND status IN (?, ?)", order.ID, StatusPending, StatusExpired).Updates(map[string]any{
		"status": StatusPaid, "payment_trade_no": notification.TradeNo, "paid_at": now, "updated_at": now,
	}).Error
	if err != nil {
		return fmt.Errorf("update order to paid: %w", err)
	}

	// Execute fulfillment (credit balance)
	if err := s.executeFulfillment(ctx, order.ID, order.UserID, order.Amount); err != nil {
		return fmt.Errorf("execute fulfillment: %w", err)
	}

	return nil
}

// executeFulfillment credits user balance via host ledger API
func (s *OrderService) executeFulfillment(ctx context.Context, orderID, userID int64, amount decimal.Decimal) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	// Update order status to recharging
	err = orders(db).Where("id = ? AND status = ?", orderID, StatusPaid).Updates(map[string]any{
		"status": StatusRecharging, "updated_at": time.Now(),
	}).Error
	if err != nil {
		return fmt.Errorf("update to recharging: %w", err)
	}

	// Credit balance via host ledger. The host prefixes the key with
	// "plugin:payment:" and applies it at most once.
	resp, err := s.credit(ctx, pluginsdk.LedgerChange{
		UserID:         userID,
		Amount:         amount.String(),
		RefType:        "payment_order",
		RefID:          strconv.FormatInt(orderID, 10),
		IdempotencyKey: fmt.Sprintf("payment_order_%d", orderID),
		Note:           fmt.Sprintf("充值 ¥%s", amount.String()),
	})
	if err != nil {
		// Mark order as failed
		_ = orders(db).Where("id = ?", orderID).Updates(map[string]any{
			"status": StatusFailed, "failed_at": time.Now(), "failed_reason": err.Error(), "updated_at": time.Now(),
		}).Error
		return fmt.Errorf("credit ledger: %w", err)
	}

	// Mark order as completed
	err = orders(db).Where("id = ?", orderID).Updates(map[string]any{
		"status": StatusCompleted, "updated_at": time.Now(),
	}).Error
	if err != nil {
		return fmt.Errorf("update to completed: %w", err)
	}

	slog.Info("payment fulfilled",
		"order_id", orderID,
		"user_id", userID,
		"amount", amount.String(),
		"ledger_id", resp.LedgerID,
		"duplicate", resp.Duplicate)

	return nil
}

// checkPendingLimit checks if user has too many pending orders
func (s *OrderService) checkPendingLimit(db *gorm.DB, userID int64, maxPending int) error {
	if maxPending <= 0 {
		return nil
	}

	var count int64
	err := orders(db).Where("user_id = ? AND status = ?", userID, StatusPending).Count(&count).Error
	if err != nil {
		return fmt.Errorf("check pending orders: %w", err)
	}

	if count >= int64(maxPending) {
		return fmt.Errorf("too many pending orders: %d (max %d)", count, maxPending)
	}

	return nil
}

// loadConfig loads payment configuration
func (s *OrderService) loadConfig(db *gorm.DB) (*PaymentConfig, error) {
	var row model.Config
	err := db.Select(`enabled, min_amount, max_amount, daily_limit, order_timeout_minutes,
		max_pending_orders, enabled_payment_types, balance_disabled,
		balance_recharge_multiplier, recharge_fee_rate, load_balance_strategy`).
		Take(&row, model.ConfigID).Error
	if err != nil {
		return nil, fmt.Errorf("load payment config: %w", err)
	}
	return &PaymentConfig{
		Enabled:                   row.Enabled,
		MinAmount:                 row.MinAmount,
		MaxAmount:                 row.MaxAmount,
		DailyLimit:                row.DailyLimit,
		OrderTimeoutMinutes:       row.OrderTimeoutMinutes,
		MaxPendingOrders:          row.MaxPendingOrders,
		EnabledPaymentTypes:       row.EnabledPaymentTypes,
		BalanceDisabled:           row.BalanceDisabled,
		BalanceRechargeMultiplier: row.BalanceRechargeMultiplier,
		RechargeFeeRate:           row.RechargeFeeRate,
		LoadBalanceStrategy:       row.LoadBalanceStrategy,
	}, nil
}

// orders selects from payment_orders.
func orders(db *gorm.DB) *gorm.DB { return db.Model(&model.Order{}) }

// PaymentNotification represents parsed payment notification
type PaymentNotification struct {
	OutTradeNo string
	TradeNo    string
	Amount     decimal.Decimal
	Status     string
	RawData    string
}

// ListOrders lists user's payment orders
func (s *OrderService) ListOrders(ctx context.Context, userID int64, limit, offset int) ([]Order, int, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, 0, err
	}
	return listPaged(db, pagedQuery{
		columns:    selectOrders,
		where:      "user_id = ?",
		args:       []any{userID},
		orderBy:    "created_at DESC",
		countLabel: "count orders",
		queryLabel: "query orders",
	}, limit, offset, func(o model.Order) Order {
		return Order{
			ID:             o.ID,
			UserID:         o.UserID,
			OutTradeNo:     o.OutTradeNo,
			PaymentType:    o.PaymentType,
			PaymentTradeNo: deref(o.PaymentTradeNo), // NULL until the order is paid
			Amount:         o.Amount,
			PayAmount:      o.PayAmount,
			Currency:       o.Currency,
			OrderType:      o.OrderType,
			Status:         o.Status,
			ExpiresAt:      o.ExpiresAt,
			PaidAt:         o.PaidAt,
			CreatedAt:      o.CreatedAt,
		}
	})
}

// selectOrders is the column list of the order list statements.
const selectOrders = `id, user_id, out_trade_no, payment_type, payment_trade_no,
	amount, pay_amount, currency, order_type, status,
	expires_at, paid_at, created_at`
