package iam

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the iam statements built with GORM (see package gormtest): every
// write, and the reads that authenticate a request, a login or a step-up.

var sqlAt = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)

// read finishes a read for its snapshot.
func read(q *gorm.DB) *gorm.DB {
	var row struct{}
	return q.Scan(&row)
}

// snap asserts each named statement as a subtest.
func snap(t *testing.T, cases map[string]func(*gorm.DB) *gorm.DB) {
	t.Helper()
	for name, build := range cases {
		t.Run(name, func(t *testing.T) { gormtest.Assert(t, build) })
	}
}

// The reads of every request: a session's user (on a status cache miss), an
// access key, and the user a login or GET /me loads.
func TestSQLRequestAuth(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"user_status":   func(db *gorm.DB) *gorm.DB { return read(db.Raw(userStatus, int64(7))) },
		"access_key":    func(db *gorm.DB) *gorm.DB { return read(db.Raw(accessKeyByHash, "hash")) },
		"touch_key":     func(db *gorm.DB) *gorm.DB { return touchAccessKey(db, 5, "203.0.113.9") },
		"user_by_id":    func(db *gorm.DB) *gorm.DB { return read(db.Raw(userByID, int64(7))) },
		"user_by_email": func(db *gorm.DB) *gorm.DB { return read(userByEmail(db, "a@example.com", "id, password_hash, status")) },
		"token_version": func(db *gorm.DB) *gorm.DB { return read(userTokenVersion(db, 7)) },
	})
}

// The user row locks and reads of logins, refreshes and step-ups.
func TestSQLUserLocks(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"live_user_for_update": func(db *gorm.DB) *gorm.DB {
			return read(liveUser(db, 7, "password_hash, status").Clauses(store.ForUpdate))
		},
		"lock_user":         func(db *gorm.DB) *gorm.DB { return read(lockUser(db, 7)) },
		"lock_user_if_any":  func(db *gorm.DB) *gorm.DB { return db.Exec(lockUserIfAny, int64(7)) },
		"factors":           func(db *gorm.DB) *gorm.DB { return read(userFactors(db, 7)) },
		"has_two_factor":    func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, totpOf(db, 7))) },
		"bootstrap_lock":    func(db *gorm.DB) *gorm.DB { return db.Exec(bootstrapLock) },
		"bootstrap_exists":  func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, liveUsers(db))) },
		"touch_last_login":  func(db *gorm.DB) *gorm.DB { return touchLastLogin(db, 7) },
		"bump_tokenversion": func(db *gorm.DB) *gorm.DB { return bumpTokenVersion(db, 7) },
	})
}

// users.updated_at moves with these writes, and only with them.
func TestSQLUserWrites(t *testing.T) {
	name, status := "Ann", StatusDisabled
	segment := int64(3)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return read(db.Raw(insertUser, "a@example.com", "Ann", "$2a$hash", &segment))
		},
		"insert_default_segment": func(db *gorm.DB) *gorm.DB {
			return read(db.Raw(insertUser, "a@example.com", "", "$2a$hash", (*int64)(nil)))
		},
		"update_all": func(db *gorm.DB) *gorm.DB {
			return updateUser(db, 7, userChanges(UpdateUserInput{DisplayName: &name, Status: &status}, "b@example.com", []byte("$2a$new")))
		},
		"update_status": func(db *gorm.DB) *gorm.DB {
			return updateUser(db, 7, userChanges(UpdateUserInput{Status: &status}, "", nil))
		},
		"delete":          func(db *gorm.DB) *gorm.DB { return updateUser(db, 7, userDeletion()) },
		"delete_api_keys": func(db *gorm.DB) *gorm.DB { return deleteUserKeys(db, 7) },
		"password":        func(db *gorm.DB) *gorm.DB { return setLivePassword(db, 7, "$2a$new") },
		"profile":         func(db *gorm.DB) *gorm.DB { return setLiveProfile(db, 7, "") },
	})
}

func TestSQLUserReads(t *testing.T) {
	filter := usersFilter(ListUsersFilter{Query: "ann", Status: StatusActive, Role: "admin", Segment: "3"})
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"list_count": func(db *gorm.DB) *gorm.DB {
			var n int64
			return db.Table("users u").Scopes(filter.Scope()).Count(&n)
		},
		"list_page": func(db *gorm.DB) *gorm.DB { return read(usersPage(db, filter, 2, 20)) },
		"list_personal": func(db *gorm.DB) *gorm.DB {
			return read(usersPage(db, usersFilter(ListUsersFilter{Segment: "personal"}), 1, 20))
		},
		"lookup": func(db *gorm.DB) *gorm.DB { return read(lookupUsers(db, []int64{3, 1, 3})) },
	})
}

// Refresh token rotation, replay revocation and logout.
func TestSQLRefreshTokens(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return db.Exec(insertRefreshToken, int64(7), "hash", sqlAt, "family")
		},
		"owner":           func(db *gorm.DB) *gorm.DB { return read(refreshByHash(db, "hash").Select("user_id")) },
		"lock":            func(db *gorm.DB) *gorm.DB { return read(lockRefreshToken(db, "hash")) },
		"live_family":     func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, liveFamily(db, "family"))) },
		"revoke_token":    func(db *gorm.DB) *gorm.DB { return revokeToken(db, 9) },
		"revoke_family":   func(db *gorm.DB) *gorm.DB { return revokeFamily(db, "family") },
		"rotate":          func(db *gorm.DB) *gorm.DB { return rotateRefresh(db, 9) },
		"logout_family":   func(db *gorm.DB) *gorm.DB { return read(liveFamilyOf(db, "hash", 7)) },
		"session_family":  func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, userFamily(db, 7, "family"))) },
		"revoke_all":      func(db *gorm.DB) *gorm.DB { return revokeSessions(db, 7, allSessions) },
		"revoke_one":      func(db *gorm.DB) *gorm.DB { return revokeSessions(db, 7, oneSession("family")) },
		"revoke_others":   func(db *gorm.DB) *gorm.DB { return revokeSessions(db, 7, otherSessions("mine")) },
		"sessions_delete": func(db *gorm.DB) *gorm.DB { return deleteSessions(db, 7, otherSessions("mine")) },
	})
}

func TestSQLLoginSessions(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"delete_stale": func(db *gorm.DB) *gorm.DB { return deleteStaleSessions(db, 7, sqlAt) },
		"upsert": func(db *gorm.DB) *gorm.DB {
			return db.Exec(upsertLoginSession, "family", int64(7), loginMethodPassword, "203.0.113.9", "Mozilla/5.0")
		},
		"list": func(db *gorm.DB) *gorm.DB { return read(liveSessions(db, 7)) },
	})
}

func TestSQLFlows(t *testing.T) {
	owner := int64(7)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"delete_expired": deleteExpiredFlows,
		"insert": func(db *gorm.DB) *gorm.DB {
			return db.Exec(insertFlow, "hash", flowLogin2FA, &owner, []byte(`{"email":"a"}`), sqlAt)
		},
		"insert_no_owner": func(db *gorm.DB) *gorm.DB {
			return db.Exec(insertFlow, "hash", flowPasskeyLogin, (*int64)(nil), []byte(`{}`), sqlAt)
		},
		"live": func(db *gorm.DB) *gorm.DB { return read(liveFlow(db, "token", flowLogin2FA, "payload, user_id")) },
		"live_owned": func(db *gorm.DB) *gorm.DB {
			return read(liveFlow(db, "token", flowTOTPSetup, "payload").Where("user_id = ?", 7))
		},
		"take": func(db *gorm.DB) *gorm.DB {
			return read(db.Raw(takeFlowSQL, "hash", flowStepUpProof, int64(7), int64(7)))
		},
		"open_step_up_session": func(db *gorm.DB) *gorm.DB {
			return db.Exec(openStepUpSession, "hash", flowStepUpSession, int64(7), []byte(`{"scope":"x"}`), sqlAt)
		},
		"continue_step_up_session": func(db *gorm.DB) *gorm.DB { return continueStepUpSession(db, "hash", sqlAt, 1800) },
	})
}

// Two-factor: the secret, its lockout and the backup codes.
func TestSQLTwoFactor(t *testing.T) {
	step := int64(59_000_000)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return db.Exec(insertTOTP, int64(7), []byte{0x01, 0x02, 0x03}, step)
		},
		"lock":            func(db *gorm.DB) *gorm.DB { return read(lockTOTP(db, 7)) },
		"passed_totp":     func(db *gorm.DB) *gorm.DB { return totpPassed(db, 7, &step) },
		"passed_backup":   func(db *gorm.DB) *gorm.DB { return totpPassed(db, 7, nil) },
		"failed":          func(db *gorm.DB) *gorm.DB { return totpFailed(db, 7, sqlAt) },
		"delete":          func(db *gorm.DB) *gorm.DB { return totpOf(db, 7).Delete(&model.UserTOTP{}) },
		"backup_insert":   func(db *gorm.DB) *gorm.DB { return db.Exec(insertBackupCode, int64(7), "$2a$code") },
		"backup_delete":   func(db *gorm.DB) *gorm.DB { return deleteBackupCodes(db, 7) },
		"backup_unused":   func(db *gorm.DB) *gorm.DB { return read(unusedBackupCodes(db, 7)) },
		"backup_use":      func(db *gorm.DB) *gorm.DB { return useBackupCode(db, 3) },
		"passkeys_delete": func(db *gorm.DB) *gorm.DB { return deletePasskeysOf(db, 7) },
	})
}

// Passkeys: credential_id and credential are bytes, one parameter each.
func TestSQLPasskeys(t *testing.T) {
	cred := []byte(`{"id":"AQID"}`)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return read(db.Raw(insertPasskey, int64(7), "Laptop", "example.com", []byte{0x01, 0x02}, cred))
		},
		"by_credential": func(db *gorm.DB) *gorm.DB { return read(passkeyByCredential(db, []byte{0x01, 0x02})) },
		"credentials":   func(db *gorm.DB) *gorm.DB { return read(passkeyCredentials(db, 7)) },
		"list":          func(db *gorm.DB) *gorm.DB { return read(passkeyList(db, 7)) },
		"use_login": func(db *gorm.DB) *gorm.DB {
			return usePasskey(db.Where("id = ? AND user_id = ?", 4, 7), cred)
		},
		"use_step_up": func(db *gorm.DB) *gorm.DB {
			return usePasskey(db.Where("user_id = ? AND credential_id = ?", 7, []byte{0x01, 0x02}), cred)
		},
		"rename": func(db *gorm.DB) *gorm.DB { return read(renamePasskey(db, 4, 7, "Phone")) },
		"delete": func(db *gorm.DB) *gorm.DB { return deletePasskey(db, 4, 7) },
	})
}

func TestSQLAccessKeys(t *testing.T) {
	name := "ci"
	expires := sqlAt
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return read(db.Raw(insertAccessKey, int64(7), "ci", "hash", "abcd", store.Strings{"account:read:own"}, &expires))
		},
		"rename":      func(db *gorm.DB) *gorm.DB { return read(updateAccessKey(db, 5, 7, &name, nil)) },
		"permissions": func(db *gorm.DB) *gorm.DB { return read(updateAccessKey(db, 5, 7, nil, []string{"account:read:own"})) },
		"delete":      func(db *gorm.DB) *gorm.DB { return deleteAccessKey(db, 5, 7) },
	})
}
