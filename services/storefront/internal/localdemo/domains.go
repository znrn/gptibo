package localdemo

import (
	"context"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/store"
	"net"
	"strings"
)

// Domain status simulates ownership, routing and TLS. It never changes public DNS.
type DomainProvider struct{}

func (DomainProvider) Ensure(_ context.Context, host string) (domains.Hostname, error) {
	h := domains.Hostname{ID: "LOCAL-" + host, Hostname: host, Status: "active"}
	h.SSL.Status = "active"
	return h, nil
}
func (p DomainProvider) Find(c context.Context, h string) (domains.Hostname, error) {
	return p.Ensure(c, h)
}
func (p DomainProvider) Get(c context.Context, id string) (domains.Hostname, error) {
	if !strings.HasPrefix(id, "LOCAL-") {
		return domains.Hostname{}, domains.ErrProviderMissing
	}
	return p.Ensure(c, strings.TrimPrefix(id, "LOCAL-"))
}
func (DomainProvider) Delete(context.Context, string) error { return nil }

type DomainDNS struct {
	Store  *store.Store
	Target string
}

func (d DomainDNS) LookupTXT(ctx context.Context, host string) ([]string, error) {
	var token string
	e := d.Store.DB.QueryRow(ctx, `SELECT verification FROM partner_domains WHERE hostname=$1`, strings.TrimPrefix(host, "_gptibo-verify.")).Scan(&token)
	return []string{token}, e
}
func (d DomainDNS) LookupCNAME(context.Context, string) (string, error) { return d.Target, nil }
func (d DomainDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil
}
