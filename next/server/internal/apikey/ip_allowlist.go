package apikey

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

func parseIPRange(value string) (netip.Prefix, error) {
	if addr, err := netip.ParseAddr(value); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("ambiguous IPv4-mapped prefix")
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

func normalizeAllowedIPs(ctx context.Context, entries []string) ([]string, error) {
	invalid := core.InvalidFields(core.FieldError{Field: "allowed_ips", Code: "invalid", Message: t(ctx,
		"Use at most 256 IPv4, IPv6 or CIDR entries", "最多填写 256 个 IPv4、IPv6 地址或 CIDR 网段")})
	if len(entries) > 256 {
		return nil, invalid
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		prefix, err := parseIPRange(strings.TrimSpace(entry))
		if err != nil {
			return nil, invalid
		}
		value := prefix.String()
		if prefix.Bits() == prefix.Addr().BitLen() {
			value = prefix.Addr().String()
		}
		if !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	return out, nil
}

func ipAllowed(client string, entries []string) bool {
	if len(entries) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(client)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	allowed := false
	for _, entry := range entries {
		prefix, err := parseIPRange(entry)
		if err != nil {
			return false
		}
		allowed = allowed || prefix.Contains(addr)
	}
	return allowed
}

func (s *Service) updateIPAllowlist(c *gin.Context) {
	ctx := c.Request.Context()
	uid, _ := core.UserID(ctx)
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	var in struct {
		AllowedIPs *[]string `json:"allowed_ips"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if in.AllowedIPs == nil {
		httpapi.Fail(c, core.InvalidFields(core.FieldError{Field: "allowed_ips", Code: "required", Message: t(ctx, "IP allowlist is required; use [] to remove restrictions", "请提供 IP 白名单；传入 [] 可取消限制")}))
		return
	}
	entries, err := normalizeAllowedIPs(ctx, *in.AllowedIPs)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	var hash string
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if hash, err = updateOwned(ctx, tx, id, uid, map[string]any{"allowed_ips": store.Strings(entries)}); err != nil {
			return err
		}
		return auditKey(ctx, tx, uid, id, "apikey.ip_allowlist", c.ClientIP(), nil)
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.dropCache(ctx, hash)
	httpapi.OK(c, gin.H{"allowed_ips": entries})
}
