package iam

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const (
	accessKeyMaxPerUser  = 20
	accessKeyMinLifetime = time.Hour
	accessKeyTouchEvery  = time.Minute
)

// AccessKey is the API view of an access key (CONTRACTS §80). Lost lists the
// chosen permissions the user no longer holds: the key cannot use them, and
// gets them back if the user does.
type AccessKey struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Hint        string     `json:"hint"`
	Permissions []string   `json:"permissions"`
	Lost        []string   `json:"lost"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	LastUsedIP  string     `json:"last_used_ip"`
	CreatedAt   time.Time  `json:"created_at"`
	// Key is the secret: only in the response that creates it.
	Key string `json:"key,omitempty"`
}

// CreateAccessKeyInput is the body of POST /me/access-keys.
type CreateAccessKeyInput struct {
	Name        string     `json:"name"`
	Permissions []string   `json:"permissions"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// UpdateAccessKeyInput is the body of PATCH /me/access-keys/:id.
type UpdateAccessKeyInput struct {
	Name        *string  `json:"name"`
	Permissions []string `json:"permissions"`
}

func normalizeAccessKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return "", core.InvalidFields(core.FieldError{Field: "name", Code: "invalid", Message: "1 to 64 characters"})
	}
	return name, nil
}

// grantablePermissions checks a key's permissions against what the user holds
// right now, the rule of §80: a key is a subset of its user, scope by scope.
// It returns them canonical, sorted, without duplicates. A superuser may
// choose any active permission.
func (s *Service) grantablePermissions(ctx context.Context, userID int64, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return nil, core.InvalidFields(core.FieldError{Field: "permissions", Code: "required", Message: "choose at least one permission"})
	}
	set, err := s.authz.PermissionSet(ctx, userID)
	if err != nil {
		return nil, err
	}
	active, err := s.authz.ActivePermissionKeys(ctx)
	if err != nil {
		return nil, err
	}
	var invalid, missing []string
	for _, k := range wanted {
		if _, err := perm.Parse(k); err != nil {
			invalid = append(invalid, k)
		}
	}
	if len(invalid) > 0 {
		return nil, core.InvalidFields(core.FieldError{Field: "permissions", Code: "invalid", Message: "invalid permission keys: " + strings.Join(invalid, ", ")})
	}
	// Canonical keys, each implied by the user's (CONTRACTS §21: a key's
	// scope never reaches beyond its user's).
	out := perm.CanonicalSet(wanted)
	for _, k := range out {
		p, _ := perm.Parse(k)
		if !slices.Contains(active, string(p.Action)) || !set.Has(k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, core.ErrPermissionDenied.WithMessage(t(ctx,
			"an access key can only have permissions you hold",
			"访问密钥只能拥有你自己的权限")).WithDetails(map[string]any{"missing": missing})
	}
	return out, nil
}

func (s *Service) lostPermissions(ctx context.Context, userID int64, keys []*AccessKey) error {
	set, err := s.authz.PermissionSet(ctx, userID)
	if err != nil {
		return err
	}
	for _, k := range keys {
		k.Lost = []string{}
		for _, p := range k.Permissions {
			if !set.Has(p) {
				k.Lost = append(k.Lost, p)
			}
		}
	}
	return nil
}

const accessKeyColumns = `id, name, token_hint, permissions, expires_at, last_used_at, last_used_ip, created_at`

// accessKeyRow is a row of accessKeyColumns.
type accessKeyRow struct {
	ID          int64         `gorm:"column:id"`
	Name        string        `gorm:"column:name"`
	TokenHint   string        `gorm:"column:token_hint"`
	Permissions store.Strings `gorm:"column:permissions"`
	ExpiresAt   *time.Time    `gorm:"column:expires_at"`
	LastUsedAt  *time.Time    `gorm:"column:last_used_at"`
	LastUsedIP  string        `gorm:"column:last_used_ip"`
	CreatedAt   time.Time     `gorm:"column:created_at"`
}

func (r *accessKeyRow) key() *AccessKey {
	return &AccessKey{ID: r.ID, Name: r.Name, Hint: r.TokenHint, Permissions: r.Permissions, ExpiresAt: r.ExpiresAt,
		LastUsedAt: r.LastUsedAt, LastUsedIP: r.LastUsedIP, CreatedAt: r.CreatedAt}
}

// accessKeysOf are the access keys of userID.
func accessKeysOf(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.UserAccessKey{}).Where("user_id = ?", userID)
}

// accessKeyList selects the accessKeyColumns of the keys of userID, newest
// first.
func accessKeyList(db *gorm.DB, userID int64) *gorm.DB {
	return accessKeysOf(db, userID).Select(accessKeyColumns).Order("created_at DESC")
}

// AccessKeys lists the user's keys.
func (s *Service) AccessKeys(ctx context.Context, userID int64) ([]*AccessKey, error) {
	var rows []accessKeyRow
	if err := accessKeyList(store.G(ctx, s.db.Gorm), userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*AccessKey, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].key())
	}
	return out, s.lostPermissions(ctx, userID, out)
}

// CreateAccessKey checks the permissions and creates a key (the route takes
// a self.access_key.write step-up). The secret appears only in this result.
func (s *Service) CreateAccessKey(ctx context.Context, userID int64, in CreateAccessKeyInput) (*AccessKey, error) {
	name, err := normalizeAccessKeyName(in.Name)
	if err != nil {
		return nil, err
	}
	if in.ExpiresAt != nil && in.ExpiresAt.Before(time.Now().Add(accessKeyMinLifetime)) {
		return nil, core.InvalidFields(core.FieldError{Field: "expires_at", Code: "too_soon", Message: "at least one hour from now"})
	}
	perms, err := s.grantablePermissions(ctx, userID, in.Permissions)
	if err != nil {
		return nil, err
	}
	raw, hash := tokenstore.NewToken(core.AccessKeyPrefix)
	var key *AccessKey
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockUserRow(ctx, tx, userID); err != nil {
			return err
		}
		g := store.G(ctx, tx)
		var n int64
		if err := accessKeysOf(g, userID).Count(&n).Error; err != nil {
			return err
		}
		if n >= accessKeyMaxPerUser {
			return core.ErrConflict.WithMessage(t(ctx, "too many access keys", "访问密钥数量已达上限"))
		}
		var row accessKeyRow
		if err := insertAccessKey(g, userID, name, hash, raw[len(raw)-4:], perms, in.ExpiresAt).Scan(&row).Error; err != nil {
			return err
		}
		key = row.key()
		return audit.Audit(ctx, tx, userID, "user.access_key.create", "user", strconv.FormatInt(userID, 10),
			map[string]any{"access_key_id": key.ID, "name": name, "permissions": perms})
	})
	if err != nil {
		return nil, err
	}
	key.Key = raw
	key.Lost = []string{}
	return key, nil
}

// insertAccessKey creates an access key and returns its accessKeyColumns.
func insertAccessKey(db *gorm.DB, userID int64, name, hash, hint string, perms []string, expiresAt *time.Time) *gorm.DB {
	return db.Raw(`INSERT INTO user_access_keys (user_id, name, token_hash, token_hint, permissions, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING `+accessKeyColumns, userID, name, hash, hint, store.Strings(perms), expiresAt)
}

// UpdateAccessKey renames a key or replaces its permissions (the route
// takes a self.access_key.write step-up, as creating a key does).
func (s *Service) UpdateAccessKey(ctx context.Context, userID, id int64, in UpdateAccessKeyInput) (*AccessKey, error) {
	var name *string
	if in.Name != nil {
		v, err := normalizeAccessKeyName(*in.Name)
		if err != nil {
			return nil, err
		}
		name = &v
	}
	var perms []string
	if in.Permissions != nil {
		var err error
		if perms, err = s.grantablePermissions(ctx, userID, in.Permissions); err != nil {
			return nil, err
		}
	}
	var key *AccessKey
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var row accessKeyRow
		res := updateAccessKey(store.G(ctx, tx), id, userID, name, perms).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return core.ErrNotFound
		}
		key = row.key()
		return audit.Audit(ctx, tx, userID, "user.access_key.update", "user", strconv.FormatInt(userID, 10),
			map[string]any{"access_key_id": id, "name": key.Name, "permissions": key.Permissions})
	})
	if err != nil {
		return nil, err
	}
	return key, s.lostPermissions(ctx, userID, []*AccessKey{key})
}

// updateAccessKey renames the key id of userID or replaces its permissions;
// a nil name or perms keeps what the key has.
func updateAccessKey(db *gorm.DB, id, userID int64, name *string, perms []string) *gorm.DB {
	return db.Raw(`UPDATE user_access_keys SET name = COALESCE(?, name), permissions = COALESCE(?, permissions)
			WHERE id = ? AND user_id = ? RETURNING `+accessKeyColumns, name, store.NullStrings(perms), id, userID)
}

// DeleteAccessKey revokes a key at once.
func (s *Service) DeleteAccessKey(ctx context.Context, userID, id int64) error {
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		res := deleteAccessKey(store.G(ctx, tx), id, userID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return core.ErrNotFound
		}
		return audit.Audit(ctx, tx, userID, "user.access_key.delete", "user", strconv.FormatInt(userID, 10), map[string]any{"access_key_id": id})
	})
}

// deleteAccessKey removes the access key id of userID.
func deleteAccessKey(db *gorm.DB, id, userID int64) *gorm.DB {
	return db.Where("id = ? AND user_id = ?", id, userID).Delete(&model.UserAccessKey{})
}

// AccessKeyPermissions is the catalog a user chooses from: the self module,
// which every user holds, then the active core and plugin modules of the
// permissions they hold, with only those.
func (s *Service) AccessKeyPermissions(ctx context.Context, userID int64) ([]*authz.PermissionModule, error) {
	set, err := s.authz.PermissionSet(ctx, userID)
	if err != nil {
		return nil, err
	}
	modules, err := s.authz.ListPermissions(ctx)
	if err != nil {
		return nil, err
	}
	out := []*authz.PermissionModule{authz.SelfPermissionModule()}
	for _, m := range modules {
		kept := m.Permissions[:0:0]
		for _, p := range m.Permissions {
			// Offered when the user holds the action with any scope; the
			// key's scope must then be within the user's (grantablePermissions).
			if (p.Status == "active" || p.Status == "") && set.HasAction(perm.Key(p.Key)) {
				kept = append(kept, p)
			}
		}
		if len(kept) > 0 && (m.Status == "active" || m.Status == "") {
			m.Permissions = kept
			out = append(out, m)
		}
	}
	return out, nil
}

// VerifyAccessKey implements core.AccessKeyVerifier. The key must exist and be
// unexpired, and its user active. What it may do is decided per check by
// authz, which intersects the key's permissions with the user's current ones.
func (s *Service) VerifyAccessKey(ctx context.Context, raw, ip string) (core.AccessKeyIdentity, error) {
	var id core.AccessKeyIdentity
	var expiresAt, lastUsed *time.Time
	g := store.G(ctx, s.db.Gorm)
	err := accessKeyByHash(g, tokenstore.Hash(raw)).Row().
		Scan(&id.KeyID, &id.UserID, (*store.Strings)(&id.Permissions), &expiresAt, &lastUsed)
	if store.IsNoRows(err) {
		return id, core.ErrUnauthenticated.WithMessage("invalid access key")
	}
	if err != nil {
		return id, err
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return id, core.ErrUnauthenticated.WithMessage("access key has expired")
	}
	active, _, err := s.isActiveWithVersion(ctx, id.UserID)
	if err != nil {
		return id, err
	}
	if !active {
		return id, core.ErrUnauthenticated.WithMessage("user is disabled or deleted")
	}
	if lastUsed == nil || time.Since(*lastUsed) > accessKeyTouchEvery {
		if err := touchAccessKey(g, id.KeyID, ip).Error; err != nil {
			return id, err
		}
	}
	return id, nil
}

// accessKeyByHash is the statement of VerifyAccessKey, raw (the fewest
// allocations): it runs for every request made with an access key.
func accessKeyByHash(db *gorm.DB, hash string) *gorm.DB {
	return db.Raw(`SELECT id, user_id, permissions, expires_at, last_used_at FROM user_access_keys WHERE token_hash = ?`, hash)
}

// touchAccessKey records a use of the key id, at most every
// accessKeyTouchEvery.
func touchAccessKey(db *gorm.DB, id int64, ip string) *gorm.DB {
	return db.Model(&model.UserAccessKey{}).Where("id = ?", id).
		Updates(map[string]any{"last_used_at": gorm.Expr("now()"), "last_used_ip": ip})
}
