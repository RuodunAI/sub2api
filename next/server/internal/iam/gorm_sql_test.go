package iam

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
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
		"user_status":   func(db *gorm.DB) *gorm.DB { return read(userStatus(db, 7)) },
		"access_key":    func(db *gorm.DB) *gorm.DB { return read(accessKeyByHash(db, "hash")) },
		"touch_key":     func(db *gorm.DB) *gorm.DB { return touchAccessKey(db, 5, "203.0.113.9") },
		"user_by_id":    func(db *gorm.DB) *gorm.DB { return read(userByID(db, 7)) },
		"user_by_email": func(db *gorm.DB) *gorm.DB { return read(userByEmail(db, "a@example.com", "id, password_hash, status")) },
		"token_version": func(db *gorm.DB) *gorm.DB { return read(userTokenVersion(db, 7)) },
	})
}

// The user row locks and reads of logins, refreshes and step-ups.
func TestSQLUserLocks(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"live_user":            func(db *gorm.DB) *gorm.DB { return read(liveUser(db, 7, "email, password_hash")) },
		"live_user_for_update": func(db *gorm.DB) *gorm.DB { return read(lockLiveUser(db, 7, "password_hash, status")) },
		"lock_user":            func(db *gorm.DB) *gorm.DB { return read(lockUser(db, 7)) },
		"lock_user_if_any":     func(db *gorm.DB) *gorm.DB { return lockUserIfAny(db, 7) },
		"factors":              func(db *gorm.DB) *gorm.DB { return read(userFactors(db, 7)) },
		"has_two_factor":       func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, totpOf(db, 7))) },
		"bootstrap_lock":       func(db *gorm.DB) *gorm.DB { return bootstrapLock(db) },
		"bootstrap_exists":     func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, liveUsers(db))) },
		"touch_last_login":     func(db *gorm.DB) *gorm.DB { return touchLastLogin(db, 7) },
		"bump_tokenversion":    func(db *gorm.DB) *gorm.DB { return bumpTokenVersion(db, 7, 2) },
	})
}

// users.updated_at moves with these writes, and only with them.
func TestSQLUserWrites(t *testing.T) {
	name, status := "Ann", StatusDisabled
	segment := int64(3)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return read(insertUser(db, "a@example.com", "Ann", "$2a$hash", &segment))
		},
		"insert_default_segment": func(db *gorm.DB) *gorm.DB {
			return read(insertUser(db, "a@example.com", "", "$2a$hash", nil))
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
			return usersCount(db, filter).Count(&n)
		},
		"list_page": func(db *gorm.DB) *gorm.DB { return read(usersPage(db, filter, 2, 20)) },
		// Out of bounds, the page and size are kept within httpapi.Pagination's:
		// GORM would leave out a size of 0 or less and a negative offset.
		"list_page_none":     func(db *gorm.DB) *gorm.DB { return read(usersPage(db, filter, 0, 0)) },
		"list_page_negative": func(db *gorm.DB) *gorm.DB { return read(usersPage(db, filter, -3, -5)) },
		"list_page_too_big":  func(db *gorm.DB) *gorm.DB { return read(usersPage(db, filter, 2, 1000)) },
		"list_personal": func(db *gorm.DB) *gorm.DB {
			return read(usersPage(db, usersFilter(ListUsersFilter{Segment: "personal"}), 1, 20))
		},
		"lookup": func(db *gorm.DB) *gorm.DB { return read(lookupUsers(db, []int64{3, 1, 3})) },
	})
}

// Refresh token rotation, replay revocation and logout.
func TestSQLRefreshTokens(t *testing.T) {
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert":         func(db *gorm.DB) *gorm.DB { return insertRefreshToken(db, 7, "hash", sqlAt, "family") },
		"owner":          func(db *gorm.DB) *gorm.DB { return read(refreshOwner(db, "hash")) },
		"lock":           func(db *gorm.DB) *gorm.DB { return read(lockRefreshToken(db, "hash")) },
		"live_family":    func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, liveFamily(db, "family"))) },
		"revoke_token":   func(db *gorm.DB) *gorm.DB { return revokeToken(db, 9) },
		"revoke_family":  func(db *gorm.DB) *gorm.DB { return revokeFamily(db, "family") },
		"rotate":         func(db *gorm.DB) *gorm.DB { return rotateRefresh(db, 9) },
		"logout_family":  func(db *gorm.DB) *gorm.DB { return read(liveFamilyOf(db, "hash", 7)) },
		"session_family": func(db *gorm.DB) *gorm.DB { return read(store.ExistsQuery(db, userFamily(db, 7, "family"))) },
		"family_issued": func(db *gorm.DB) *gorm.DB {
			return read(store.ExistsQuery(db, familyIssuedSince(db, 7, "family", sqlAt)))
		},
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
			return upsertLoginSession(db, "family", 7, sessionMeta{method: loginMethodPassword, ip: "203.0.113.9", userAgent: "Mozilla/5.0"})
		},
		"list": func(db *gorm.DB) *gorm.DB { return read(liveSessions(db, 7)) },
	})
}

func TestSQLFlows(t *testing.T) {
	owner := int64(7)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"delete_expired": deleteExpiredFlows,
		"insert": func(db *gorm.DB) *gorm.DB {
			return insertFlow(db, "hash", flowLogin2FA, &owner, []byte(`{"email":"a"}`), sqlAt)
		},
		"insert_no_owner": func(db *gorm.DB) *gorm.DB {
			return insertFlow(db, "hash", flowPasskeyLogin, nil, []byte(`{}`), sqlAt)
		},
		"live":       func(db *gorm.DB) *gorm.DB { return read(liveFlow(db, "token", flowLogin2FA, "payload, user_id")) },
		"live_owned": func(db *gorm.DB) *gorm.DB { return read(ownedFlow(db, "token", flowTOTPSetup, 7, "payload")) },
		// The owner is bound twice: once for the "any owner" test, once to match.
		"take":          func(db *gorm.DB) *gorm.DB { return read(takeFlowQuery(db, "hash", flowStepUpProof, 7)) },
		"take_any_user": func(db *gorm.DB) *gorm.DB { return read(takeFlowQuery(db, "hash", flowPasskeyLogin, 0)) },
		"open_step_up_session": func(db *gorm.DB) *gorm.DB {
			return openStepUpSession(db, "hash", 7, []byte(`{"scope":"x"}`), sqlAt)
		},
		"continue_step_up_session": func(db *gorm.DB) *gorm.DB { return continueStepUpSession(db, "hash", sqlAt, 1800) },
	})
}

// Two-factor: the secret, its lockout and the backup codes.
func TestSQLTwoFactor(t *testing.T) {
	step := int64(59_000_000)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert":        func(db *gorm.DB) *gorm.DB { return insertTOTP(db, 7, []byte{0x01, 0x02, 0x03}, step) },
		"lock":          func(db *gorm.DB) *gorm.DB { return read(lockTOTP(db, 7)) },
		"enabled_at":    func(db *gorm.DB) *gorm.DB { return read(totpEnabledAt(db, 7)) },
		"passed_totp":   func(db *gorm.DB) *gorm.DB { return totpPassed(db, 7, &step) },
		"passed_backup": func(db *gorm.DB) *gorm.DB { return totpPassed(db, 7, nil) },
		"failed":        func(db *gorm.DB) *gorm.DB { return totpFailed(db, 7, sqlAt) },
		"delete":        func(db *gorm.DB) *gorm.DB { return deleteTOTP(db, 7) },
		"backup_insert": func(db *gorm.DB) *gorm.DB { return insertBackupCode(db, 7, "$2a$code") },
		"backup_delete": func(db *gorm.DB) *gorm.DB { return deleteBackupCodes(db, 7) },
		"backup_unused": func(db *gorm.DB) *gorm.DB { return read(unusedBackupCodes(db, 7)) },
		"backup_left": func(db *gorm.DB) *gorm.DB {
			var n int64
			return backupCodesLeft(db, 7).Count(&n)
		},
		"backup_use":      func(db *gorm.DB) *gorm.DB { return useBackupCode(db, 3) },
		"passkeys_delete": func(db *gorm.DB) *gorm.DB { return deletePasskeysOf(db, 7) },
	})
}

// Passkeys: credential_id and credential are bytes, one parameter each.
func TestSQLPasskeys(t *testing.T) {
	cred := []byte(`{"id":"AQID"}`)
	snap(t, map[string]func(*gorm.DB) *gorm.DB{
		"insert": func(db *gorm.DB) *gorm.DB {
			return read(insertPasskey(db, 7, "Laptop", "example.com", []byte{0x01, 0x02}, cred))
		},
		"by_credential": func(db *gorm.DB) *gorm.DB { return read(passkeyByCredential(db, []byte{0x01, 0x02})) },
		"credentials":   func(db *gorm.DB) *gorm.DB { return read(passkeyCredentials(db, 7)) },
		"list":          func(db *gorm.DB) *gorm.DB { return read(passkeyList(db, 7)) },
		"use_login":     func(db *gorm.DB) *gorm.DB { return usePasskey(passkeyByID(db, 4, 7), cred) },
		"use_step_up": func(db *gorm.DB) *gorm.DB {
			return usePasskey(userPasskeyByCredential(db, 7, []byte{0x01, 0x02}), cred)
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
			return read(insertAccessKey(db, 7, "ci", "hash", "abcd", []string{"account:read:own"}, &expires))
		},
		"insert_no_permissions": func(db *gorm.DB) *gorm.DB {
			return read(insertAccessKey(db, 7, "ci", "hash", "abcd", nil, nil))
		},
		"list": func(db *gorm.DB) *gorm.DB { return read(accessKeyList(db, 7)) },
		// CreateAccessKey's check of the number of keys.
		"count": func(db *gorm.DB) *gorm.DB {
			var n int64
			return accessKeysOf(db, 7).Count(&n)
		},
		"rename":      func(db *gorm.DB) *gorm.DB { return read(updateAccessKey(db, 5, 7, &name, nil)) },
		"permissions": func(db *gorm.DB) *gorm.DB { return read(updateAccessKey(db, 5, 7, nil, []string{"account:read:own"})) },
		"delete":      func(db *gorm.DB) *gorm.DB { return deleteAccessKey(db, 5, 7) },
	})
}
