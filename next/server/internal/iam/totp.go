package iam

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const (
	totpPeriod        = 30
	totpSkewSteps     = 1 // one step either side, for a clock that is off
	totpMaxFailures   = 5
	totpLockout       = 5 * time.Minute
	backupCodeCount   = 10
	backupCodeLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
	totpIssuer        = "sup2api"
)

var errBadSecondFactor = core.InvalidFields(core.FieldError{Field: "code", Code: "incorrect", Message: "incorrect verification code"})

// TwoFactorStatus is the response of GET /me/two-factor.
type TwoFactorStatus struct {
	Enabled         bool       `json:"enabled"`
	EnabledAt       *time.Time `json:"enabled_at,omitempty"`
	BackupCodesLeft int        `json:"backup_codes_left"`
}

// TwoFactorSetup is the response of POST /me/two-factor/setup.
type TwoFactorSetup struct {
	FlowToken  string `json:"flow_token"`
	ExpiresIn  int64  `json:"expires_in"`
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauth_url"`
	QRCode     string `json:"qr_code"` // data URL of a PNG
}

func totpAAD(userID int64) []byte { return []byte("user_totp:" + strconv.FormatInt(userID, 10)) }

func (s *Service) sealTOTP(userID int64, secret string) ([]byte, error) {
	if s.cipher == nil {
		return nil, core.ErrInternal.WithMessage("two-factor authentication needs the master key")
	}
	return s.cipher.Encrypt([]byte(secret), totpAAD(userID))
}

func (s *Service) openTOTP(userID int64, sealed []byte) (string, error) {
	if s.cipher == nil {
		return "", core.ErrInternal.WithMessage("two-factor authentication needs the master key")
	}
	plain, err := s.cipher.Decrypt(sealed, totpAAD(userID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// totpMatch returns the time step the code is valid for and newer than
// lastStep. The steps are checked oldest first, so a code cannot be replayed
// by moving forward within the skew.
func totpMatch(secret, code string, lastStep int64, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	current := now.Unix() / totpPeriod
	for step := current - totpSkewSteps; step <= current+totpSkewSteps; step++ {
		if step <= lastStep {
			continue
		}
		want, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// normalizeBackupCode accepts a code typed with or without its dash, any case.
func normalizeBackupCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}

func newBackupCode() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = backupCodeLetters[int(b[i])%len(backupCodeLetters)]
	}
	return string(b[:5]) + "-" + string(b[5:]), nil
}

// issueBackupCodes replaces the user's backup codes and returns the new ones,
// which are shown once and kept only as hashes.
func (s *Service) issueBackupCodes(ctx context.Context, tx pgx.Tx, userID int64) ([]string, error) {
	g := store.G(ctx, tx)
	if err := deleteBackupCodes(g, userID).Error; err != nil {
		return nil, err
	}
	codes := make([]string, backupCodeCount)
	for i := range codes {
		code, err := newBackupCode()
		if err != nil {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(normalizeBackupCode(code)), s.cost)
		if err != nil {
			return nil, err
		}
		if err := g.Exec(insertBackupCode, userID, string(hash)).Error; err != nil {
			return nil, err
		}
		codes[i] = code
	}
	return codes, nil
}

// insertBackupCode stores the hash of a backup code, raw: Create would add a
// RETURNING of the defaulted columns.
const insertBackupCode = `INSERT INTO user_totp_backup_codes (user_id, code_hash) VALUES (?, ?)`

// deleteBackupCodes removes the backup codes of userID.
func deleteBackupCodes(db *gorm.DB, userID int64) *gorm.DB {
	return db.Where("user_id = ?", userID).Delete(&model.UserTOTPBackupCode{})
}

// useBackupCode marks the backup code id used.
func useBackupCode(db *gorm.DB, id int64) *gorm.DB {
	return db.Model(&model.UserTOTPBackupCode{}).Where("id = ?", id).Update("used_at", gorm.Expr("now()"))
}

// totpOf is the two-factor row of userID.
func totpOf(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.UserTOTP{}).Where("user_id = ?", userID)
}

// hasTwoFactor reports whether userID has two-factor enabled.
func hasTwoFactor(db *gorm.DB, userID int64) (bool, error) {
	return store.Exists(db, totpOf(db, userID))
}

// totpPassed records a correct code: the step it was of (lastStep, a TOTP
// code only) and no failures or lockout.
func totpPassed(db *gorm.DB, userID int64, lastStep *int64) *gorm.DB {
	set := map[string]any{"failed_attempts": 0, "locked_until": nil}
	store.SetIf(set, "last_step", lastStep)
	return totpOf(db, userID).Updates(set)
}

// totpFailed counts a wrong code; the totpMaxFailures-th locks the factor
// until lockUntil and starts the count again.
func totpFailed(db *gorm.DB, userID int64, lockUntil time.Time) *gorm.DB {
	return totpOf(db, userID).Updates(map[string]any{
		"failed_attempts": gorm.Expr("CASE WHEN failed_attempts + 1 >= ? THEN 0 ELSE failed_attempts + 1 END", totpMaxFailures),
		"locked_until":    gorm.Expr("CASE WHEN failed_attempts + 1 >= ? THEN ?::timestamptz ELSE locked_until END", totpMaxFailures, lockUntil),
	})
}

// checkSecondFactor verifies a TOTP or backup code inside tx, which must hold
// the user row. A correct code is used up. Failures count towards a lockout
// and are written by the caller with recordSecondFactorFailure, after the
// transaction, so they survive its rollback.
func (s *Service) checkSecondFactor(ctx context.Context, tx pgx.Tx, userID int64, code string) error {
	var sealed []byte
	var lastStep int64
	var lockedUntil *time.Time
	g := store.G(ctx, tx)
	err := lockTOTP(g, userID).Row().Scan(&sealed, &lastStep, &lockedUntil)
	if store.IsNoRows(err) {
		return core.ErrConflict.WithMessage("two-factor authentication is not enabled")
	}
	if err != nil {
		return err
	}
	if lockedUntil != nil && lockedUntil.After(time.Now()) {
		return rateLimitedError(ctx, time.Until(*lockedUntil))
	}
	secret, err := s.openTOTP(userID, sealed)
	if err != nil {
		return err
	}
	if step, ok := totpMatch(secret, code, lastStep, time.Now()); ok {
		return totpPassed(g, userID, &step).Error
	}
	normalized := normalizeBackupCode(code)
	if len(normalized) == 10 {
		var unused []model.UserTOTPBackupCode
		if err := unusedBackupCodes(g, userID).Find(&unused).Error; err != nil {
			return err
		}
		var matched int64
		for _, c := range unused {
			if matched == 0 && bcrypt.CompareHashAndPassword([]byte(c.CodeHash), []byte(normalized)) == nil {
				matched = c.ID
			}
		}
		if matched != 0 {
			if err := useBackupCode(g, matched).Error; err != nil {
				return err
			}
			return totpPassed(g, userID, nil).Error
		}
	}
	return errBadSecondFactor
}

// lockTOTP reads the two-factor state of userID, locked until the
// transaction ends.
func lockTOTP(db *gorm.DB, userID int64) *gorm.DB {
	return totpOf(db, userID).Select("secret_enc, last_step, locked_until").Clauses(store.ForUpdate)
}

// unusedBackupCodes are the backup codes of userID still to be used.
func unusedBackupCodes(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.UserTOTPBackupCode{}).Select("id, code_hash").Where("user_id = ? AND used_at IS NULL", userID)
}

// recordSecondFactorFailure counts a wrong code; the fifth locks the factor
// for five minutes. It runs outside the transaction the check failed in.
func (s *Service) recordSecondFactorFailure(ctx context.Context, userID int64) error {
	return totpFailed(store.G(ctx, s.db.Pool), userID, time.Now().Add(totpLockout)).Error
}

// TwoFactor returns the caller's two-factor state.
func (s *Service) TwoFactor(ctx context.Context, userID int64) (*TwoFactorStatus, error) {
	g := store.G(ctx, s.db.Pool)
	var at time.Time
	err := totpOf(g, userID).Select("enabled_at").Row().Scan(&at)
	if store.IsNoRows(err) {
		return &TwoFactorStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	var left int64
	err = g.Model(&model.UserTOTPBackupCode{}).Where("user_id = ? AND used_at IS NULL", userID).Count(&left).Error
	return &TwoFactorStatus{Enabled: true, EnabledAt: &at, BackupCodesLeft: int(left)}, err
}

type totpSetupPayload struct {
	Sealed string `json:"sealed"`
}

// StartTwoFactor starts enrolment: the secret stays in a flow until a code
// from it proves the authenticator app has it. Adding a factor is a step-up
// operation (self.2fa.enable, verified by the route): a password alone must
// not add a factor that then passes every step-up of a passkey-only user.
func (s *Service) StartTwoFactor(ctx context.Context, userID int64) (*TwoFactorSetup, error) {
	g := store.G(ctx, s.db.Pool)
	var email string
	err := liveUser(g, userID, "email").Row().Scan(&email)
	if store.IsNoRows(err) {
		return nil, core.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	enabled, err := hasTwoFactor(g, userID)
	if err != nil {
		return nil, err
	}
	if enabled {
		return nil, core.ErrConflict.WithMessage("two-factor authentication is already enabled")
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: email, Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealTOTP(userID, key.Secret())
	if err != nil {
		return nil, err
	}
	token, err := s.createFlow(ctx, flowTOTPSetup, userID, totpSetupPayload{Sealed: base64.StdEncoding.EncodeToString(sealed)})
	if err != nil {
		return nil, err
	}
	img, err := key.Image(240, 240)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return &TwoFactorSetup{
		FlowToken: token, ExpiresIn: flowTTLSeconds, Secret: key.Secret(), OTPAuthURL: key.URL(),
		QRCode: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// EnableTwoFactor finishes enrolment with a code from the new secret and
// returns the backup codes. A wrong code keeps the flow, so it can be retyped
// until the flow expires.
func (s *Service) EnableTwoFactor(ctx context.Context, userID int64, flowToken, code string) ([]string, error) {
	var payload totpSetupPayload
	var raw []byte
	err := liveFlow(store.G(ctx, s.db.Pool), flowToken, flowTOTPSetup, "payload").Where("user_id = ?", userID).Row().Scan(&raw)
	if store.IsNoRows(err) {
		return nil, errFlowExpired
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(payload.Sealed)
	if err != nil {
		return nil, err
	}
	secret, err := s.openTOTP(userID, sealed)
	if err != nil {
		return nil, err
	}
	step, ok := totpMatch(secret, code, 0, time.Now())
	if !ok {
		return nil, errBadSecondFactor
	}
	var codes []string
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshUser(ctx, tx, userID); err != nil {
			return err
		}
		res := store.G(ctx, tx).Exec(insertTOTP, userID, sealed, step)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return core.ErrConflict.WithMessage("two-factor authentication is already enabled")
		}
		var out totpSetupPayload
		if _, err := s.takeFlow(ctx, tx, flowToken, flowTOTPSetup, userID, &out); err != nil {
			return err
		}
		if codes, err = s.issueBackupCodes(ctx, tx, userID); err != nil {
			return err
		}
		return audit.Audit(ctx, tx, userID, "user.two_factor.enable", "user", strconv.FormatInt(userID, 10), nil)
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// insertTOTP enables two-factor with a sealed secret, unless it is; raw:
// Create would add a RETURNING of the defaulted columns. The secret's ? does
// not follow a (, where GORM would expand the bytes.
const insertTOTP = `INSERT INTO user_totp (user_id, secret_enc, last_step) VALUES (?, ?, ?) ON CONFLICT (user_id) DO NOTHING`

// DisableTwoFactor turns the factor off; the route has verified the caller
// (step-up self.2fa.disable).
func (s *Service) DisableTwoFactor(ctx context.Context, userID int64) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := s.keepLastFactor(ctx, tx, userID, core.StepUpSelf2FADisable); err != nil {
			return err
		}
		if err := requireTwoFactor(ctx, tx, userID); err != nil {
			return err
		}
		if err := removeTwoFactor(ctx, tx, userID); err != nil {
			return err
		}
		return audit.Audit(ctx, tx, userID, "user.two_factor.disable", "user", strconv.FormatInt(userID, 10), nil)
	})
}

// RegenerateBackupCodes replaces the backup codes; the route has verified
// the caller (step-up self.2fa.backup_codes).
func (s *Service) RegenerateBackupCodes(ctx context.Context, userID int64) ([]string, error) {
	var codes []string
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := requireTwoFactor(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		if codes, err = s.issueBackupCodes(ctx, tx, userID); err != nil {
			return err
		}
		return audit.Audit(ctx, tx, userID, "user.two_factor.backup_codes", "user", strconv.FormatInt(userID, 10), nil)
	})
	return codes, err
}

// requireTwoFactor fails with conflict unless userID has two-factor enabled.
func requireTwoFactor(ctx context.Context, tx pgx.Tx, userID int64) error {
	enabled, err := hasTwoFactor(store.G(ctx, tx), userID)
	if err != nil {
		return err
	}
	if !enabled {
		return core.ErrConflict.WithMessage("two-factor authentication is not enabled")
	}
	return nil
}

func removeTwoFactor(ctx context.Context, tx pgx.Tx, userID int64) error {
	g := store.G(ctx, tx)
	if err := deleteBackupCodes(g, userID).Error; err != nil {
		return err
	}
	return totpOf(g, userID).Delete(&model.UserTOTP{}).Error
}

// deletePasskeysOf removes every passkey of userID.
func deletePasskeysOf(db *gorm.DB, userID int64) *gorm.DB {
	return db.Where("user_id = ?", userID).Delete(&model.UserPasskey{})
}

// ResetSecurity is the operator's way back in for a user who lost their
// factors: it removes the user's two-factor and passkeys, only of a user the
// operator manages (authz.CanActOnUsers; oneself only as the super admin,
// FU6). Over HTTP the route has verified the caller (step-up
// user.security.reset), the super admin resetting its own factors included.
func (s *Service) ResetSecurity(ctx context.Context, actorID, targetID int64) error {
	// SEC-H2: removing the factors of a user one does not manage would leave
	// that account one password away from takeover.
	if err := s.authz.CanActOnUser(ctx, actorID, core.ActOnIdentity, targetID); err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshUser(ctx, tx, targetID); err != nil {
			return err
		}
		if err := removeTwoFactor(ctx, tx, targetID); err != nil {
			return err
		}
		if err := deletePasskeysOf(store.G(ctx, tx), targetID).Error; err != nil {
			return err
		}
		return audit.Audit(ctx, tx, actorID, "user.security.reset", "user", strconv.FormatInt(targetID, 10), nil)
	})
}

// confirmPassword checks the caller's password for a sensitive change, under
// the login rate limit, and returns the caller's email.
func (s *Service) confirmPassword(ctx context.Context, userID int64, password, ip string) (string, error) {
	var email, hash string
	err := liveUser(store.G(ctx, s.db.Pool), userID, "email, password_hash").Row().Scan(&email, &hash)
	if store.IsNoRows(err) {
		return "", core.ErrUnauthenticated
	}
	if err != nil {
		return "", err
	}
	if wait := s.limiter.check(ctx, email, ip); wait > 0 {
		return "", rateLimitedError(ctx, wait)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.limiter.fail(ctx, email, ip)
		return "", core.InvalidFields(core.FieldError{Field: "password", Code: "incorrect", Message: "incorrect password"})
	}
	return email, nil
}

func isBadSecondFactor(err error) bool { return errors.Is(err, errBadSecondFactor) }
