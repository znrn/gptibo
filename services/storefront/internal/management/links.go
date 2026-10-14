package management

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var referralCode = regexp.MustCompile(`^(?:[A-Z0-9]{6}|[A-Z]{8})$`)

// Use verified bindings, independent of the hostname used to open the dashboard.
func (s Service) partnerLinks(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, fallback string) (string, string, error) {
	var slug, ownHost, parentHost string
	err := q.QueryRow(ctx, `SELECT p.slug,COALESCE(own.hostname,''),COALESCE(upstream.hostname,'') FROM partners p
 LEFT JOIN partners parent ON parent.id=p.parent_id AND parent.active
 LEFT JOIN LATERAL (SELECT d.hostname FROM partner_domains d JOIN partner_stores shop ON shop.id=d.store_id WHERE d.partner_id=p.id AND p.active AND shop.slot=1 AND shop.active AND d.state='active' AND d.checked_at>now()-interval '24 hours' AND (d.free OR d.expires_at+interval '3 days'>now()) ORDER BY d.free DESC,d.created_at,d.id LIMIT 1) own ON true
 LEFT JOIN LATERAL (SELECT d.hostname FROM partner_domains d JOIN partner_stores shop ON shop.id=d.store_id WHERE d.partner_id=parent.id AND shop.active AND d.state='active' AND d.checked_at>now()-interval '24 hours' AND (d.free OR d.expires_at+interval '3 days'>now()) ORDER BY (shop.slot=1) DESC,d.free DESC,d.created_at,d.id LIMIT 1) upstream ON true
 WHERE p.id=$1`, id).Scan(&slug, &ownHost, &parentHost)
	if err != nil {
		return "", "", err
	}
	origin := strings.TrimRight(fallback, "/")
	path := "/s/" + url.PathEscape(slug)
	if referralCode.MatchString(slug) {
		path = "/r/" + slug
	}
	if !s.PlatformLinksOnly && s.Domains != nil && s.Domains.Enabled() {
		if ownHost != "" {
			origin, path = "https://"+ownHost, "/"
		} else if parentHost != "" {
			origin = "https://" + parentHost
		}
	}
	return origin + path, origin + "/join", nil
}
func (s Service) attachPartnerLinks(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, p map[string]any) error {
	shop, login, err := s.partnerLinks(ctx, q, p["id"].(string), s.Origin)
	if err != nil {
		return err
	}
	p["storefront_url"], p["login_url"] = shop, login
	return nil
}
