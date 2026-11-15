package payment

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// errorMessage is the message of an error response.
func errorMessage(t *testing.T, resp *pluginv1.HTTPResponse) string {
	t.Helper()
	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(resp.GetBody(), &body); err != nil {
		t.Fatalf("%d %s: %v", resp.GetStatus(), resp.GetBody(), err)
	}
	return body.Error.Message
}

// TestAdminErrorText: the console shows a database error as the database
// worded it, after the endpoint's own prefix ("insert code: ERROR: ..."):
// not with the text GORM's error translation puts in front of it.
func TestAdminErrorText(t *testing.T) {
	p, _ := startDB(t)
	ctx := context.Background()
	db := mustDB(t, p)
	h := p.httpHandler

	// Every code collides with the first one.
	if err := db.Exec(`CREATE UNIQUE INDEX redeem_codes_one ON redeem_codes ((true))`).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(GenerateCodesRequest{Count: 2, Type: RedeemTypeBalance, Value: decimal.NewFromInt(1)})
	resp, err := h.handleGenerateRedeemCodes(ctx, &pluginv1.HTTPRequest{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	msg := errorMessage(t, resp)
	if want := `insert code: ERROR: duplicate key value violates unique constraint "redeem_codes_one" (SQLSTATE 23505)`; resp.GetStatus() != 400 || msg != want {
		t.Errorf("generate: %d %q, want 400 %q", resp.GetStatus(), msg, want)
	}

	// A check violation of an UPDATE (GORM translates the errors of its
	// writes).
	promo, err := p.promoService.CreatePromoCode(ctx, CreatePromoCodeRequest{Code: "P1", BonusAmount: decimal.NewFromInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`ALTER TABLE promo_codes ADD CONSTRAINT promo_codes_status_known CHECK (status IN ('active', 'disabled'))`).Error; err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"status": "bogus", "max_uses": 1})
	req := &pluginv1.HTTPRequest{Body: body, PathParams: map[string]string{"id": strconv.FormatInt(promo.ID, 10)}}
	if resp, err = h.handleUpdatePromoCode(ctx, req); err != nil {
		t.Fatal(err)
	}
	msg = errorMessage(t, resp)
	if want := `update promo code: ERROR: new row for relation "promo_codes" violates check constraint "promo_codes_status_known" (SQLSTATE 23514)`; resp.GetStatus() != 400 || msg != want {
		t.Errorf("update promo: %d %q, want 400 %q", resp.GetStatus(), msg, want)
	}

	// An unknown column fails the page statement of an admin list.
	if err := db.Exec(`ALTER TABLE redeem_codes DROP COLUMN notes`).Error; err != nil {
		t.Fatal(err)
	}
	resp, err = h.handleListRedeemCodes(ctx, &pluginv1.HTTPRequest{})
	if err != nil {
		t.Fatal(err)
	}
	msg = errorMessage(t, resp)
	if !strings.HasPrefix(msg, `query codes: ERROR: column "notes" does not exist`) || strings.Contains(msg, gorm.ErrInvalidField.Error()) {
		t.Errorf("list: %d %q", resp.GetStatus(), msg)
	}
}
