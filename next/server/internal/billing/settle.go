package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The ledger's row lock on user_balances serialises every charge of one user,
// and the lock is held until COMMIT. Every client round trip made while it is
// held is paid by everyone else queueing for the same user at a distant
// PostgreSQL (CONTRACTS §63.7). The statements below therefore do a whole
// ledger change in one statement each, a settlement (release and charge)
// included, and skip the balance row's creation for a user who has one:
// ApplyTx and SettleTx are one statement, then the events, then the
// caller's COMMIT.
//
// They run through GORM (store.G) on the caller's transaction. The CTE
// statements keep pgx's numbered placeholders, which name each parameter
// once however often the CTEs use it; ledgerStmt rewrites them for GORM
// once, at start-up.
//
// Creating the balance row on demand. A data-modifying CTE's rows are
// invisible to the rest of its statement, so the row a statement locks must
// exist before it starts, and ensureBalanceSQL used to run first, always.
// For a user whose row the statement's snapshot holds, that INSERT ... ON
// CONFLICT DO NOTHING finds the committed row and does nothing: no row, no
// foreign key check, no row lock. So the statements report whether they
// found the row (the column "present": they locked it, or wanted no lock and
// saw it) and the caller runs ensureBalanceSQL and the statement again only
// when they did not. A statement that did not find the row wrote nothing and
// holds no row lock (every write reads the locked row), so that path is the
// old sequence, ensureBalanceSQL then the statement, after a read.

// ensureBalanceSQL creates the balance row a later statement locks, when the
// statement reported the row missing. An unknown user fails here with the
// users foreign key (store.IsForeignKeyViolation → not found).
const ensureBalanceSQL = `INSERT INTO user_balances (user_id) VALUES (?::bigint) ON CONFLICT DO NOTHING`

// ensureBalance runs ensureBalanceSQL for the user on db.
func ensureBalance(db *gorm.DB, userID int64) error {
	err := db.Exec(ensureBalanceSQL, userID).Error
	if store.IsForeignKeyViolation(err) {
		return core.ErrNotFound.WithMessage("user not found").WithCause(err)
	}
	return err
}

// applySQL is ApplyTx in one statement: find an existing ledger row for the
// idempotency key; otherwise lock the balance row, insert the ledger row with
// the new balance and update the balance. Parameters: 1 user, 2 signed delta,
// 3 kind, 4 ref_type, 5 ref_id, 6 idempotency key, 7 operator id (NULL), 8
// plugin key (” → NULL), 9 note. Columns: applied id and balance_after (0
// when nothing was written), whether the key existed before the statement
// and that row's id, balance_after, user_id, delta and kind (applyRow), and
// whether the balance row was present (see above). Nothing applied and no
// existing row means another transaction inserted the key after this
// statement's snapshot (ON CONFLICT DO NOTHING); the caller re-reads it.
const applySQL = `
WITH existing AS (
    SELECT id, balance_after, user_id, delta, kind FROM balance_ledger WHERE idempotency_key = $6::varchar
), locked AS (
    SELECT balance FROM user_balances
    WHERE user_id = $1::bigint AND NOT EXISTS (SELECT 1 FROM existing)
    FOR UPDATE
), ledger AS (
    INSERT INTO balance_ledger (user_id, delta, balance_after, kind, ref_type, ref_id,
        idempotency_key, operator_id, plugin_key, note)
    SELECT $1::bigint, $2::numeric, locked.balance + $2::numeric, $3::text, $4::text, $5::text,
        $6::varchar, $7::bigint, NULLIF($8::text, ''), $9::text
    FROM locked
    ON CONFLICT (idempotency_key) DO NOTHING
    RETURNING id, balance_after
), applied AS (
    UPDATE user_balances SET balance = ledger.balance_after, updated_at = now()
    FROM ledger
    WHERE user_balances.user_id = $1::bigint
    RETURNING ledger.id, ledger.balance_after
)
SELECT COALESCE((SELECT id FROM applied), 0),
       COALESCE((SELECT balance_after FROM applied), 0),
       EXISTS (SELECT 1 FROM existing),
       COALESCE((SELECT id FROM existing), 0),
       COALESCE((SELECT balance_after FROM existing), 0),
       COALESCE((SELECT user_id FROM existing), 0),
       COALESCE((SELECT delta FROM existing), 0),
       COALESCE((SELECT kind FROM existing), ''),
       EXISTS (SELECT 1 FROM locked)
           OR (EXISTS (SELECT 1 FROM existing) AND EXISTS (SELECT 1 FROM user_balances WHERE user_id = $1::bigint))`

var applyStmt = newLedgerStmt(applySQL)

// releaseSQL is ReleasePrechargeTx in one statement. The balance row is
// locked first and the reservation deleted under that lock (USING locked), the
// lock order the ledger and the expiry sweeper share; the lock is not taken
// when the request has no reservation. The refund ledger row is written with
// the reservation's amount unless its idempotency key already exists.
// Parameters: 1 request id, 2 user, 3 idempotency key, 4 note. Columns: the
// refund's id and balance_after (0 when none was written), whether a
// reservation was deleted and its amount (releaseRow).
const releaseSQL = `
WITH locked AS (
    SELECT balance FROM user_balances
    WHERE user_id = $2::bigint
      AND EXISTS (SELECT 1 FROM request_precharges WHERE request_id = $1::varchar AND user_id = $2::bigint)
    FOR UPDATE
), released AS (
    DELETE FROM request_precharges p USING locked
    WHERE p.request_id = $1::varchar AND p.user_id = $2::bigint
    RETURNING p.amount
), ledger AS (
    INSERT INTO balance_ledger (user_id, delta, balance_after, kind, ref_type, ref_id,
        idempotency_key, operator_id, plugin_key, note)
    SELECT $2::bigint, r.amount, l.balance + r.amount, 'refund', 'precharge', $1::varchar,
        $3::varchar, NULL, NULL, $4::text
    FROM released r, locked l
    WHERE NOT EXISTS (SELECT 1 FROM balance_ledger WHERE idempotency_key = $3::varchar)
    ON CONFLICT (idempotency_key) DO NOTHING
    RETURNING id, balance_after
), applied AS (
    UPDATE user_balances SET balance = ledger.balance_after, updated_at = now()
    FROM ledger
    WHERE user_balances.user_id = $2::bigint
    RETURNING ledger.id, ledger.balance_after
)
SELECT COALESCE((SELECT id FROM applied), 0),
       COALESCE((SELECT balance_after FROM applied), 0),
       EXISTS (SELECT 1 FROM released),
       COALESCE((SELECT amount FROM released), 0)`

var releaseStmt = newLedgerStmt(releaseSQL)

// settleSQL is SettleTx in one statement: releaseSQL for the request's
// reservation, then applySQL for its charge (when $5), as one. It replaces
// the two statements SettleTx ran under the balance lock, which cost a round
// trip each while the lock was held.
//
// Why it is the same as releaseSQL followed by applySQL:
//   - The lock. The balance row is locked FOR UPDATE once, when either
//     statement locked it: a reservation is visible (releaseSQL's
//     condition) or a charge whose key is not (applySQL's). Every write reads
//     the locked row, so nothing is written before the lock, and the
//     reservation is deleted under it (USING locked), as before.
//   - The snapshot. Every CTE sees the statement's snapshot. releaseSQL's
//     reads are unchanged. applySQL's one read, the charge key (existing),
//     used to be taken after the release's lock and is now taken before it;
//     a key that commits in between was "existing" and is now a lost ON
//     CONFLICT race (charge empty, existing empty), which the caller already
//     answers with the same row and the same duplicate check
//     (resolveApply), the way ApplyTx always has.
//   - The amounts. The charge's balance_after is the refund's balance_after
//     plus the delta, or the locked balance plus the delta when no refund
//     was written: the balance applySQL read after releaseSQL's update.
//     numeric addition is exact, so the values are those two statements
//     wrote.
//   - The order. The charge reads the refund's RETURNING row (LEFT JOIN
//     refund), so the refund is inserted (and takes its id) before the
//     charge row is formed: ids still ascend refund, charge.
//   - The balance. A row cannot be updated twice in one statement, so the
//     two UPDATEs are one (applied), to the charge's balance_after when it
//     was written, else to the refund's: the value the second of them left.
//     updated_at = now() is the transaction's time either way. applied
//     returns both ledger rows, so a row is reported only with its balance
//     update, as each statement's applied did.
//
// Parameters: 1 user, 2 request id, 3 refund idempotency key, 4 refund note,
// 5 whether there is a charge, 6 its signed delta, 7 kind, 8 ref_type,
// 9 ref_id, 10 idempotency key, 11 operator id, 12 plugin key (” → NULL),
// 13 note. Columns: releaseRow's, applyRow's for the charge, and present.
const settleSQL = `
WITH existing AS (
    SELECT id, balance_after, user_id, delta, kind FROM balance_ledger
    WHERE $5::boolean AND idempotency_key = $10::varchar
), reservation AS (
    SELECT 1 FROM request_precharges WHERE request_id = $2::varchar AND user_id = $1::bigint
), locked AS (
    SELECT balance FROM user_balances
    WHERE user_id = $1::bigint
      AND (EXISTS (SELECT 1 FROM reservation) OR ($5::boolean AND NOT EXISTS (SELECT 1 FROM existing)))
    FOR UPDATE
), released AS (
    DELETE FROM request_precharges p USING locked
    WHERE p.request_id = $2::varchar AND p.user_id = $1::bigint
    RETURNING p.amount
), refund AS (
    INSERT INTO balance_ledger (user_id, delta, balance_after, kind, ref_type, ref_id,
        idempotency_key, operator_id, plugin_key, note)
    SELECT $1::bigint, r.amount, l.balance + r.amount, 'refund', 'precharge', $2::varchar,
        $3::varchar, NULL, NULL, $4::text
    FROM released r, locked l
    WHERE NOT EXISTS (SELECT 1 FROM balance_ledger WHERE idempotency_key = $3::varchar)
    ON CONFLICT (idempotency_key) DO NOTHING
    RETURNING id, balance_after
), charge AS (
    INSERT INTO balance_ledger (user_id, delta, balance_after, kind, ref_type, ref_id,
        idempotency_key, operator_id, plugin_key, note)
    SELECT $1::bigint, $6::numeric, COALESCE(r.balance_after, l.balance) + $6::numeric, $7::text, $8::text,
        $9::text, $10::varchar, $11::bigint, NULLIF($12::text, ''), $13::text
    FROM locked l LEFT JOIN refund r ON true
    WHERE $5::boolean AND NOT EXISTS (SELECT 1 FROM existing)
    ON CONFLICT (idempotency_key) DO NOTHING
    RETURNING id, balance_after
), applied AS (
    UPDATE user_balances SET balance = COALESCE(c.balance_after, r.balance_after), updated_at = now()
    FROM (SELECT 1) one LEFT JOIN refund r ON true LEFT JOIN charge c ON true
    WHERE user_balances.user_id = $1::bigint AND (r.id IS NOT NULL OR c.id IS NOT NULL)
    RETURNING r.id AS refund_id, r.balance_after AS refund_after, c.id AS charge_id, c.balance_after AS charge_after
)
SELECT COALESCE((SELECT refund_id FROM applied), 0),
       COALESCE((SELECT refund_after FROM applied), 0),
       EXISTS (SELECT 1 FROM released),
       COALESCE((SELECT amount FROM released), 0),
       COALESCE((SELECT charge_id FROM applied), 0),
       COALESCE((SELECT charge_after FROM applied), 0),
       EXISTS (SELECT 1 FROM existing),
       COALESCE((SELECT id FROM existing), 0),
       COALESCE((SELECT balance_after FROM existing), 0),
       COALESCE((SELECT user_id FROM existing), 0),
       COALESCE((SELECT delta FROM existing), 0),
       COALESCE((SELECT kind FROM existing), ''),
       EXISTS (SELECT 1 FROM locked)
           OR (NOT EXISTS (SELECT 1 FROM reservation)
               AND (NOT $5::boolean OR EXISTS (SELECT 1 FROM existing))
               AND EXISTS (SELECT 1 FROM user_balances WHERE user_id = $1::bigint))`

var settleStmt = newLedgerStmt(settleSQL)

// ledgerStmt is a ledger statement rewritten for GORM: each $n of its SQL is
// a ? that binds the n-th argument, again wherever $n repeats.
type ledgerStmt struct {
	sql  string
	args []int // for each ?, in order, the index of the argument it binds
}

// newLedgerStmt rewrites query, which must hold no ? or @ of its own (GORM
// reads both as placeholders) and no $ but its placeholders.
func newLedgerStmt(query string) ledgerStmt {
	if strings.ContainsAny(query, "?@") {
		panic("billing: a ledger statement holds a ? or @")
	}
	var b strings.Builder
	var st ledgerStmt
	for i := 0; i < len(query); i++ {
		j := i + 1
		for query[i] == '$' && j < len(query) && query[j] >= '0' && query[j] <= '9' {
			j++
		}
		if j == i+1 {
			b.WriteByte(query[i])
			continue
		}
		n, _ := strconv.Atoi(query[i+1 : j])
		if n < 1 {
			panic("billing: a ledger statement holds $" + query[i+1:j])
		}
		st.args = append(st.args, n-1)
		b.WriteByte('?')
		i = j - 1
	}
	st.sql = b.String()
	return st
}

// row runs the statement on db with args ($1 is args[0]) and returns its
// one row.
func (st ledgerStmt) row(db *gorm.DB, args ...any) *sql.Row {
	return st.raw(db, args...).Row()
}

// raw is the statement on db with args, not yet run.
func (st ledgerStmt) raw(db *gorm.DB, args ...any) *gorm.DB {
	vars := make([]any, len(st.args))
	for i, n := range st.args {
		vars[i] = args[n]
	}
	return db.Raw(st.sql, vars...)
}

func releaseKey(requestID string) string { return "precharge_release:" + requestID }

// applyRow is what applySQL reports of a change, and settleSQL of its charge.
type applyRow struct {
	applied core.LedgerResult // the new ledger row; LedgerID 0 when none was written
	existed bool              // the key's row was there before the statement
	dup     core.LedgerResult // that row, when existed
}

// dest is the row's scan destinations, in the statements' column order.
func (a *applyRow) dest() []any {
	return []any{&a.applied.LedgerID, &a.applied.BalanceAfter, &a.existed,
		&a.dup.LedgerID, &a.dup.BalanceAfter, &a.dup.UserID, &a.dup.Delta, &a.dup.Kind}
}

// resolve turns the row into ApplyTx's result for ch: the new row, or the
// existing one after the duplicate check, re-reading the key after a lost
// insert race.
func (a *applyRow) resolve(ctx context.Context, tx pgx.Tx, ch core.LedgerChange) (*core.LedgerResult, error) {
	switch {
	case a.applied.LedgerID != 0:
		return &a.applied, nil
	case a.existed:
		a.dup.Duplicate = true
		return checkDuplicate(&a.dup, ch)
	}
	// Neither a new nor an existing row: another transaction inserted the
	// key after this statement's snapshot (ON CONFLICT DO NOTHING).
	dup, err := findLedger(ctx, tx, ch.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if dup == nil {
		return nil, core.ErrInternal.WithMessage("idempotency key vanished during insert")
	}
	return checkDuplicate(dup, ch)
}

// applyArgs are applySQL's parameters for a validated, rounded change.
func applyArgs(ch core.LedgerChange) []any {
	delta := ch.Amount
	if !ch.Credit {
		delta = delta.Neg()
	}
	return []any{ch.UserID, delta, ch.Kind, ch.RefType, ch.RefID, ch.IdempotencyKey, ch.OperatorID, ch.PluginKey, ch.Note}
}

// applyChange runs applySQL for a validated, rounded change on db, creating
// the balance row first when the statement reports it missing.
func applyChange(db *gorm.DB, ch core.LedgerChange) (applyRow, error) {
	var a applyRow
	var present bool
	run := func() error { return applyStmt.row(db, applyArgs(ch)...).Scan(append(a.dest(), &present)...) }
	if err := run(); err != nil || present {
		return a, err
	}
	if err := ensureBalance(db, ch.UserID); err != nil {
		return a, err
	}
	err := run()
	return a, err
}

// checkDuplicate is the idempotency contract: the same key must carry the
// same user, amount and kind.
func checkDuplicate(dup *core.LedgerResult, ch core.LedgerChange) (*core.LedgerResult, error) {
	delta := ch.Amount
	if !ch.Credit {
		delta = delta.Neg()
	}
	if dup.UserID != ch.UserID || !dup.Delta.Equal(delta) || dup.Kind != ch.Kind {
		return nil, core.ErrConflict.WithMessage("idempotency key reused with different user, amount or kind")
	}
	return dup, nil
}

// releaseRow is what releaseSQL reports of a request's reservation, and
// settleSQL of its release.
type releaseRow struct {
	refund   core.LedgerResult // the refund's ledger row; LedgerID 0 when none was written
	released bool              // a reservation was deleted
	amount   decimal.Decimal   // its amount
}

// dest is the row's scan destinations, in the statements' column order.
func (r *releaseRow) dest() []any {
	return []any{&r.refund.LedgerID, &r.refund.BalanceAfter, &r.released, &r.amount}
}

// resolve returns the refund; nil means the request had no reservation. A
// reservation that was deleted while its refund key already existed
// (nothing new written) is reported through the existing ledger row, as
// ApplyTx's duplicate.
func (r *releaseRow) resolve(ctx context.Context, tx pgx.Tx, key string) (*core.LedgerResult, error) {
	if !r.released {
		return nil, nil
	}
	if r.refund.LedgerID != 0 {
		return &r.refund, nil
	}
	dup, err := findLedger(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if dup == nil {
		return nil, core.ErrInternal.WithMessage("precharge released without a refund row")
	}
	return dup, nil
}

// releasePrecharge runs releaseSQL for the request's reservation on tx and
// returns the refund and its amount; nil means the request had no
// reservation.
func releasePrecharge(ctx context.Context, tx pgx.Tx, userID int64, requestID string) (*core.LedgerResult, decimal.Decimal, error) {
	key := releaseKey(requestID)
	var r releaseRow
	if err := releaseStmt.row(store.G(ctx, tx), requestID, userID, key, "release request precharge").Scan(r.dest()...); err != nil {
		return nil, r.amount, err
	}
	res, err := r.resolve(ctx, tx, key)
	return res, r.amount, err
}

func balanceChanged(userID int64, res *core.LedgerResult, delta decimal.Decimal, kind string) core.Event {
	return core.Event{Type: core.EventBalanceChanged, Payload: map[string]any{
		"user_id": userID, "ledger_id": res.LedgerID, "delta": delta, "balance_after": res.BalanceAfter, "kind": kind,
	}}
}

// settleRow is what settleSQL reports.
type settleRow struct {
	release releaseRow
	charge  applyRow
}

// settleArgs are settleSQL's parameters: the release's, then the charge's
// as applySQL takes them (without the user, $1 already), or inert values
// when there is no charge.
func settleArgs(userID int64, requestID string, charge *core.LedgerChange) []any {
	args := []any{userID, requestID, releaseKey(requestID), "release request precharge", charge != nil}
	if charge == nil {
		return append(args, decimal.Zero, "", "", "", "", (*int64)(nil), "", "")
	}
	return append(args, applyArgs(*charge)[1:]...)
}

// settleLedger runs settleSQL for the request's release and the validated,
// rounded charge (nil: release only) on db, creating the balance row first
// when the statement reports it missing.
func settleLedger(db *gorm.DB, userID int64, requestID string, charge *core.LedgerChange) (settleRow, error) {
	args := settleArgs(userID, requestID, charge)
	var st settleRow
	var present bool
	run := func() error {
		return settleStmt.row(db, args...).Scan(append(append(st.release.dest(), st.charge.dest()...), &present)...)
	}
	if err := run(); err != nil || present {
		return st, err
	}
	if err := ensureBalance(db, userID); err != nil {
		return st, err
	}
	err := run()
	return st, err
}

// SettleTx releases the request's precharge (if any) and applies the final
// charge (if any) inside the caller's transaction, in one statement under
// the user's balance lock (settleSQL), then emits the balance.changed events
// in one call. It returns the charge's result, or the refund's when there is
// no charge (nil when nothing moved). The caller refreshes the balance cache
// after committing, as for ApplyTx.
func (s *Service) SettleTx(ctx context.Context, tx pgx.Tx, userID int64, requestID string, charge *core.LedgerChange) (*core.LedgerResult, error) {
	if charge != nil {
		if err := validateChange(charge); err != nil {
			return nil, err
		}
		charge.Amount = charge.Amount.Round(8)
		if charge.UserID != userID {
			return nil, errors.New("settlement charge belongs to another user")
		}
	}
	st, err := settleLedger(store.G(ctx, tx), userID, requestID, charge)
	if err != nil {
		return nil, err
	}
	refunded, err := st.release.resolve(ctx, tx, releaseKey(requestID))
	if err != nil {
		return nil, err
	}
	var charged *core.LedgerResult
	if charge != nil {
		if charged, err = st.charge.resolve(ctx, tx, *charge); err != nil {
			return nil, err
		}
	}
	if s.events != nil {
		var evs []core.Event
		if refunded != nil && !refunded.Duplicate {
			evs = append(evs, balanceChanged(userID, refunded, st.release.amount, KindRefund))
		}
		if charged != nil && !charged.Duplicate {
			delta := charge.Amount
			if !charge.Credit {
				delta = delta.Neg()
			}
			evs = append(evs, balanceChanged(userID, charged, delta, charge.Kind))
		}
		if len(evs) > 0 {
			if err := s.events.Emit(ctx, tx, evs...); err != nil {
				return nil, fmt.Errorf("emit balance.changed: %w", err)
			}
		}
	}
	if charge != nil {
		return charged, nil
	}
	return refunded, nil
}

// isLockTimeout reports PostgreSQL's lock_not_available (lock_timeout hit).
func isLockTimeout(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}
