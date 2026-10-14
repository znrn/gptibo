package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/security"
)

func TestHierarchyCustomDomainLoginStoreAndLinks(t *testing.T) {
	owner, app, a, _, mail := identityFixture(t)
	ctx := context.Background()
	execSQL(t, owner, `INSERT INTO partners(id,slug,name,email,active,can_recruit) VALUES('parent','PAR123','Parent','parent@example.test',true,true),('other','OTH123','Other','other@example.test',true,true);
 INSERT INTO partners(id,slug,name,email,active,parent_id) VALUES('child','CHI123','Child','child@example.test',true,'parent'),('sibling','SIB123','Sibling','sibling@example.test',true,'parent');
 INSERT INTO partner_domains(partner_id,hostname,verification,state,checked_at) VALUES('parent','parent.example.com','fixture','active',now()),('other','other.example.com','fixture','active',now());
 INSERT INTO catalog_plans(id,product_id,name,price_cents,enabled,partner_enabled) VALUES('plus','chatgpt','Plus',16000,true,true);
 INSERT INTO partner_prices(partner_id,plan,price_cents,cost_cents) VALUES('parent','plus',18000,10000),('child','plus',20000,18000),('sibling','plus',21000,18000);`)
	svc := &domains.Service{Store: app, Provider: &domainProvider{}, DNS: &domainDNS{}, Target: "shops.shop.example.invalid", Zone: "shop.example.invalid", Limit: 100}
	h := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: a, OrderTesting: true, Domains: svc, TrustedProxies: []string{"127.0.0.1/32"}})
	call := func(host, shop, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Storefront-Host", host)
		r.Header.Set("X-Storefront-Shop", shop)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://"+host)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	signIn := func(host, shop, role, email string) *http.Cookie {
		t.Helper()
		execSQL(t, owner, `DELETE FROM auth_limits`)
		prefix := "/api/partner/auth"
		if role == "customer" {
			prefix = "/api/orders/recovery"
		}
		id := object(t, call(host, shop, "POST", prefix+"/send", map[string]any{"email": email}), 200)["challenge"].(string)
		in := map[string]any{"email": email, "challenge": id, "code": mail.get(id).Code}
		status := call("other.example.com", shop, "POST", prefix+"/verify", in).Code
		if status != 401 && status != 404 {
			t.Fatal("challenge accepted on another domain", status)
		}
		response := call(host, shop, "POST", prefix+"/verify", in)
		object(t, response, 200)
		for _, c := range response.Result().Cookies() {
			if c.Name == "__Host-sf_"+role {
				if c.Domain != "" || !c.Secure || !c.HttpOnly {
					t.Fatal("unsafe cookie")
				}
				return c
			}
		}
		t.Fatal("missing session")
		return nil
	}
	parent := signIn("parent.example.com", "", "partner", "parent@example.test")
	child := signIn("parent.example.com", "", "partner", "child@example.test")
	if call("parent.example.com", "", "GET", "/api/admin/session", nil, parent).Code != 404 {
		t.Fatal("admin exposed on custom domain")
	}
	object(t, request(h, "GET", "/api/partner/dashboard", nil, child), 401)
	object(t, call("other.example.com", "", "GET", "/api/partner/dashboard", nil, child), 401)
	// Another merchant receives the same generic send response but no code.
	execSQL(t, owner, `DELETE FROM auth_limits`)
	unknown := object(t, call("parent.example.com", "", "POST", "/api/partner/auth/send", map[string]any{"email": "other@example.test"}), 200)["challenge"].(string)
	if mail.get(unknown).Code != "" {
		t.Fatal("foreign partner received a code")
	}
	dashboard := func() map[string]any {
		return object(t, call("parent.example.com", "", "GET", "/api/partner/dashboard", nil, child), 200)["partner"].(map[string]any)
	}
	if p := dashboard(); p["id"] != "child" || p["storefront_url"] != "https://parent.example.com/r/CHI123" || p["login_url"] != "https://parent.example.com/join" {
		t.Fatal("wrong inherited links", p)
	}
	p := object(t, call("parent.example.com", "", "GET", "/api/partner/children/child", nil, parent), 200)["partner"].(map[string]any)
	if p["storefront_url"] != "https://parent.example.com/r/CHI123" {
		t.Fatal("parent child link", p)
	}
	object(t, call("parent.example.com", "", "GET", "/api/partner/children/sibling", nil, child), 404)
	object(t, call("parent.example.com", "OTH123", "GET", "/api/catalog", nil), 404)
	object(t, call("parent.example.com", "", "GET", "/api/internal/domain-route?partner=OTH123", nil), 404)
	catalog := object(t, call("parent.example.com", "CHI123", "GET", "/api/catalog", nil), 200)
	if catalog["partner"].(map[string]any)["slug"] != "CHI123" {
		t.Fatal("child catalog became parent")
	}
	buyer := &http.Cookie{Name: "__Host-sf_buyer", Value: security.Token()}
	create := func(shop string, price int) string {
		in := map[string]any{"plan": "plus", "email": "buyer@example.test", "key": security.ID(), "expectedPrice": price, "partner": "OTH123"}
		return object(t, call("parent.example.com", shop, "POST", "/api/orders", in, buyer), 201)["order"].(map[string]any)["id"].(string)
	}
	childOrder, parentOrder := create("CHI123", 20000), create("", 18000)
	var childID, upstream string
	var price, cost, upstreamCost int
	if e := owner.DB.QueryRow(ctx, `SELECT partner_id,upstream_partner_id,price_cents,cost_cents,upstream_cost_cents FROM orders WHERE id=$1`, childOrder).Scan(&childID, &upstream, &price, &cost, &upstreamCost); e != nil || childID != "child" || upstream != "parent" || price != 20000 || cost != 18000 || upstreamCost != 10000 {
		t.Fatal("child financial snapshot", childID, upstream, price, cost, upstreamCost, e)
	}
	object(t, call("parent.example.com", "CHI123", "GET", "/api/orders/"+childOrder, nil, buyer), 200)
	object(t, call("parent.example.com", "", "GET", "/api/orders/"+childOrder, nil, buyer), 404)
	object(t, call("parent.example.com", "SIB123", "GET", "/api/orders/"+childOrder, nil, buyer), 404)
	object(t, call("parent.example.com", "CHI123", "GET", "/api/orders/"+parentOrder, nil, buyer), 404)
	recovered := signIn("parent.example.com", "CHI123", "customer", "buyer@example.test")
	recoveredBrowser := &http.Cookie{Name: "__Host-sf_buyer", Value: security.Token()}
	object(t, call("parent.example.com", "CHI123", "GET", "/api/orders/"+childOrder, nil, recovered, recoveredBrowser), 200)
	object(t, call("parent.example.com", "", "GET", "/api/orders/"+parentOrder, nil, recovered, recoveredBrowser), 404)
	object(t, call("parent.example.com", "CHI123", "POST", "/api/visitors", map[string]any{"visitorId": security.ID(), "path": "/r/CHI123/product/chatgpt"}), 200)
	var visits int
	owner.DB.QueryRow(ctx, `SELECT count(*) FROM visitor_days WHERE shop_id='child'`).Scan(&visits)
	if visits != 1 {
		t.Fatal("visitor attributed to parent")
	}
	execSQL(t, owner, `INSERT INTO partner_domains(partner_id,hostname,verification,state,checked_at) VALUES('child','child.example.com','fixture','active',now())`)
	if p := dashboard(); p["storefront_url"] != "https://child.example.com/" || p["login_url"] != "https://child.example.com/join" {
		t.Fatal("own domain not preferred", p)
	}
	ownChild := signIn("child.example.com", "", "partner", "child@example.test")
	object(t, call("child.example.com", "", "GET", "/api/partner/dashboard", nil, ownChild), 200)
	object(t, call("child.example.com", "PAR123", "GET", "/api/catalog", nil), 404)
	execSQL(t, owner, `UPDATE partner_domains SET checked_at=now()-interval '2 days' WHERE partner_id='child'`)
	if p := dashboard(); p["storefront_url"] != "https://parent.example.com/r/CHI123" {
		t.Fatal("stale own domain not skipped", p)
	}
	execSQL(t, owner, `UPDATE partner_domains SET state='deleting' WHERE partner_id='parent'`)
	object(t, call("parent.example.com", "", "GET", "/api/partner/dashboard", nil, child), 404)
	// Main-site access remains available and chooses the platform fallback.
	execSQL(t, owner, `DELETE FROM auth_limits`)
	platform := login(t, h, mail, "partner", "child@example.test")
	p = object(t, request(h, "GET", "/api/partner/dashboard", nil, platform), 200)["partner"].(map[string]any)
	if p["storefront_url"] != testOrigin+"/r/CHI123" {
		t.Fatal("platform fallback", p)
	}
	execSQL(t, owner, `UPDATE partner_domains SET state='active' WHERE partner_id='parent'; UPDATE partners SET parent_id='other' WHERE id='child'`)
	object(t, call("parent.example.com", "", "GET", "/api/partner/dashboard", nil, child), 401)
	object(t, call("parent.example.com", "CHI123", "GET", "/api/catalog", nil), 404)
}

func TestPlatformLinksKeepSimulatedDomainsOnLocalOrigin(t *testing.T) {
	owner, app, a, _, mail := identityFixture(t)
	execSQL(t, owner, `INSERT INTO partners(id,slug,name,email,active) VALUES('local','LOC123','Local','local@example.test',true);
 INSERT INTO partner_domains(partner_id,hostname,verification,state,checked_at) VALUES('local','local.example.com','fixture','active',now())`)
	svc := &domains.Service{Store: app, Provider: &domainProvider{}, DNS: &domainDNS{}, Target: "shops.example.invalid", Zone: "platform.example.invalid", Limit: 100}
	h := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: a, Domains: svc, PlatformLinksOnly: true})
	cookie := login(t, h, mail, "partner", "local@example.test")
	p := object(t, request(h, "GET", "/api/partner/dashboard", nil, cookie), 200)["partner"].(map[string]any)
	if p["storefront_url"] != testOrigin+"/r/LOC123" || p["login_url"] != testOrigin+"/join" {
		t.Fatal("simulated domain leaked into preview links", p)
	}
	shops := object(t, request(h, "GET", "/api/partner/stores", nil, cookie), 200)
	if shops["referralOrigin"] != "" {
		t.Fatal("store preview left the platform origin")
	}
}
