package localdemo

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/delivery"
	"gptibo/storefront/internal/domainbilling"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/invoices"
	"gptibo/storefront/internal/management"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/procurement"
	"gptibo/storefront/internal/recharge"
	"gptibo/storefront/internal/refunds"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/securityaudit"
	"gptibo/storefront/internal/store"
	"gptibo/storefront/internal/worker"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Database        string `json:"database"`
	OwnerPassword   string `json:"ownerPassword"`
	RuntimePassword string `json:"runtimePassword"`
	Secret          string `json:"secret"`
	TOTP            string `json:"totp"`
	LaunchKey       string `json:"launchKey"`
	Origin          string `json:"origin"`
	Listen          string `json:"listen"`
	Dist            string `json:"dist"`
	State           string `json:"state"`
	DBPort          string `json:"dbPort"`
}

func (c Config) Validate() error {
	host, port, e := net.SplitHostPort(c.Listen)
	u, ue := url.Parse(c.Origin)
	if e != nil || host != "127.0.0.1" || port == "" || ue != nil || u.Scheme != "http" || u.Host != "localhost:"+port || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("local demo requires a loopback listener and matching localhost origin")
	}
	if c.Database != "gptibo_demo" || len(c.OwnerPassword) < 32 || len(c.RuntimePassword) < 32 || !security.ValidToken(c.LaunchKey) || !security.ValidToken(c.Secret) || !security.ValidTOTP(c.TOTP) {
		return errors.New("isolated demo database and generated credentials required")
	}
	if !filepath.IsAbs(c.State) || !filepath.IsAbs(c.Dist) {
		return errors.New("absolute state and frontend paths required")
	}
	return nil
}
func (c Config) dsn(user, password string) string {
	port := c.DBPort
	if port == "" {
		port = "5446"
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort("127.0.0.1", port), Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	return u.String()
}

type Server struct {
	Config   Config
	Store    *store.Store
	Auth     *auth.Service
	Mail     Mailbox
	Supplier Supplier
	Recharge *recharge.Service
	Payments *payments.Service
	Billing  *domainbilling.Service
	Invoices *invoices.Service
	API      http.Handler
	State    *State
}

func New(ctx context.Context, c Config, logger *slog.Logger) (*Server, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	owner, e := store.Open(ctx, c.dsn("postgres", c.OwnerPassword))
	if e != nil {
		return nil, e
	}
	defer owner.DB.Close()
	if e = owner.Migrate(ctx); e != nil {
		return nil, fmt.Errorf("migrate: %w", e)
	}
	if e = seed(ctx, owner); e != nil {
		return nil, fmt.Errorf("seed: %w", e)
	}
	if e = owner.GrantRuntime(ctx, "gptibo_demo_app", c.RuntimePassword); e != nil {
		return nil, fmt.Errorf("runtime grants: %w", e)
	}
	s, e := store.Open(ctx, c.dsn("gptibo_demo_app", c.RuntimePassword))
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		if !success {
			s.DB.Close()
		}
	}()
	if e = s.VerifyRuntime(ctx); e != nil {
		return nil, e
	}
	state, e := OpenState(c.State)
	if e != nil {
		return nil, e
	}
	mail := Mailbox{state}
	a, e := auth.New(s, auth.Config{Secret: c.Secret, AdminUsername: "local-admin", AdminEmail: "local-admin@example.invalid", AdminTOTP: c.TOTP, Sender: mail})
	if e != nil {
		return nil, e
	}
	sim := Supplier{state}
	client, e := recharge.NewClient("https://local-supplier.example.invalid/v1", "local-demo", "local-demo", c.Secret)
	if e != nil {
		return nil, e
	}
	client.HTTP = &http.Client{Transport: sim, Timeout: 5 * time.Second}
	rc := &recharge.Service{Store: s, Client: client, Checkout: true, Products: map[string]string{"plus": "plus", "pro-5x": "pro-5x", "pro-20x": "pro-20x", "pro-25x": "pro-25x"}, Notify: "https://local-supplier.example.invalid/demo-callback", CredentialSecret: c.Secret, Checker: SessionChecker{}}
	ps := &payments.Service{Store: s, Checkout: true, NotifyOrigin: "https://local-payment.example.invalid", Providers: map[string]payments.Provider{}}
	for _, id := range []string{"alipay", "gatewaya-alipay", "gatewaya-wxpay", "gatewayb-alipay", "gatewayb-wxpay"} {
		ps.Providers[id] = Payment{state, id, c.Origin}
	}
	ps.Provider = ps.Providers["alipay"]
	dlv := delivery.Service{Store: s, Auth: a, Sender: mail, Origin: c.Origin, DomainsEnabled: func() bool { return false }}
	goodsHTTP := &http.Client{Transport: GoodsTransport{state}, Timeout: 5 * time.Second}
	pc := &procurement.Service{Store: s, Auth: a, Delivery: dlv, AccountSupplier: &procurement.AccountSupplier{Base: "https://local-goods.example.invalid", App: "1", Secret: c.Secret, HTTP: goodsHTTP}, DeliverySupplier: &procurement.DeliverySupplier{Key: c.Secret, HTTP: goodsHTTP}, GmailEnabled: true, NYEnabled: true, EmailEnabled: true, PaymentReady: true, GoodsID: 1, MaxCents: 100, FX: 7}
	eligible := map[string]bool{}
	rows, e := s.DB.Query(ctx, `SELECT id FROM partners WHERE email LIKE '%@example.invalid'`)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		eligible[id] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	ds := &domains.Service{Store: s, Provider: DomainProvider{}, DNS: DomainDNS{s, "shops.example.invalid"}, Target: "shops.example.invalid", Zone: "platform.example.invalid", Limit: 100, MonthlyFeeCents: 1000, EligibilityOverrides: eligible}
	audit := &securityaudit.Service{Store: s, Auth: a, Sender: mail, Recipient: "local-admin@example.invalid"}
	gin.SetMode(gin.ReleaseMode)
	api := httpapi.Router(s, "local-demo", logger, httpapi.Options{PlatformLinksOnly: true, Origin: c.Origin, Auth: a, SecurityAudit: audit, Domains: ds, Payments: ps, PaymentTesting: true, Recharge: rc, Procurement: pc, MailSender: mail, AdminEmail: "local-admin@example.invalid"})
	server := &Server{Config: c, Store: s, Auth: a, Mail: mail, Supplier: sim, Recharge: rc, Payments: ps, API: api, State: state}
	server.Billing = &domainbilling.Service{Store: s, Payments: *ps, MonthlyFeeCents: 1000, RenewEnabled: func() bool { return true }, VerifySupplier: rc.Verify}
	server.Invoices = &invoices.Service{Store: s, Auth: a, Payments: ps, Provider: ps.Provider, NotifyOrigin: ps.NotifyOrigin}
	rf := refunds.Service{Store: s, Supplier: rc}
	jobs := []worker.Job{
		{Name: "local-domain-billing", Interval: 15 * time.Second, Timeout: 30 * time.Second, Run: server.Billing.Reconcile},
		{Name: "local-invoice-payment", Interval: 15 * time.Second, Timeout: 30 * time.Second, Run: func(ctx context.Context) error { _, e := server.Invoices.Reconcile(ctx); return e }},
		{Name: "local-procurement", Interval: 5 * time.Second, Timeout: 30 * time.Second, Run: pc.Reconcile},
		{Name: "local-recharge", Interval: 5 * time.Second, Timeout: 30 * time.Second, Run: rc.ReconcileActive},
		{Name: "local-refunds", Interval: 10 * time.Second, Timeout: 30 * time.Second, Run: rf.Reconcile},
		{Name: "local-domains", Interval: 10 * time.Second, Timeout: 30 * time.Second, Run: ds.Reconcile},
		{Name: "local-audit", Interval: time.Minute, Timeout: 30 * time.Second, Run: audit.Alerts},
		{Name: "local-mail", Interval: 10 * time.Second, Timeout: 30 * time.Second, Run: func(ctx context.Context) error { _, e := dlv.PartnerOrderNotifications(ctx, 20); return e }},
		{Name: "local-reminders", Interval: time.Minute, Timeout: 30 * time.Second, Run: func(ctx context.Context) error { _, e := dlv.Reminders(ctx, 20); return e }},
	}
	if e = s.Heartbeat(ctx, "local-demo"); e != nil {
		return nil, e
	}
	go func() { _ = worker.Run(ctx, s, "local-demo", logger, jobs...) }()
	success = true
	return server, nil
}

func seed(ctx context.Context, s *store.Store) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	plans := []struct {
		id, product, name string
		price             int64
	}{{"plus", "chatgpt", "ChatGPT Plus", 13900}, {"pro-5x", "chatgpt", "ChatGPT Pro 5x", 69900}, {"pro-20x", "chatgpt", "ChatGPT Pro 20x", 140000}, {"pro-25x", "chatgpt", "ChatGPT Pro 25x", 450000}, {"codex-sms-once", "codex", "Codex 短信验证", 1000}, {"gemini-pro-18m", "gemini", "Gemini Pro 18 个月", 1990}, {"duolingo-12m", "duolingo", "多邻国 12 个月", 1990}, {"gmail-account", "gmail", "Gmail 账号", 1880}, {"x-premium-3m", "x", "X Premium 3 个月", 1700}, {"x-premium-6m", "x", "X Premium 6 个月", 3400}}
	for i, p := range plans {
		_, e = tx.Exec(ctx, `INSERT INTO catalog_plans(id,product_id,name,price_cents,default_cost_cents,enabled,partner_enabled,purchase_note,sort_order) VALUES($1,$2,$3,$4,$5,true,true,'本机演示数据；不会实际购买、充值或交付商品。',$6) ON CONFLICT(id) DO NOTHING`, p.id, p.product, p.name, p.price, p.price*7/10, i)
		if e != nil {
			return e
		}
	}
	for _, p := range []struct{ email, name string }{{"partner@example.invalid", "演示代理"}, {"child@example.invalid", "演示下级代理"}} {
		var exists bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partners WHERE email=$1)`, p.email).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			if _, _, e = management.ProvisionPartner(ctx, tx, auth.Principal{Role: "admin", Subject: "local-demo"}, p.email, p.name); e != nil {
				return e
			}
		}
	}
	// The production partner_default_store trigger creates each default store.
	return tx.Commit(ctx)
}

func (s *Server) allowed(r *http.Request) bool {
	u, _ := url.Parse(s.Config.Origin)
	if u == nil || r.Host != u.Host {
		return false
	}
	h, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil || net.ParseIP(h) == nil || !net.ParseIP(h).IsLoopback() {
		return false
	}
	if r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("X-Storefront-Host") != "" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.Config.Origin {
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != s.Config.Origin {
		return false
	}
	return true
}
func (s *Server) admin(r *http.Request) bool {
	c, e := r.Cookie("sf_admin")
	if e != nil {
		return false
	}
	_, e = s.Auth.Require(r.Context(), s.Store.DB, "admin", c.Value)
	return e == nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) login(ctx context.Context, role, email string) (string, error) {
	cache := "session/" + role + "/" + email
	s.State.Lock()
	token := s.State.Receipts[cache]
	s.State.Unlock()
	if token != "" {
		if _, e := s.Auth.Require(ctx, s.Store.DB, role, token); e == nil {
			return token, nil
		}
	}
	username := ""
	if role == "admin" {
		username = "local-admin"
	}
	id, e := s.Auth.Send(ctx, role, auth.SendInput{Username: username, Email: email}, "127.0.0.1")
	if e != nil {
		return "", e
	}
	in := auth.VerifyInput{Username: username, Email: email, Challenge: id, EmailCode: s.Mail.Code(id)}
	if role == "admin" {
		in.Code = security.TOTP(s.Config.TOTP, time.Now().Unix()/30)
	} else {
		in.Code = s.Mail.Code(id)
	}
	token, e = s.Auth.Verify(ctx, role, in, "127.0.0.1")
	if e != nil {
		return "", e
	}
	s.State.Lock()
	defer s.State.Unlock()
	s.State.Receipts[cache] = token
	return token, s.State.save()
}
func sessionCookie(w http.ResponseWriter, role, token string) {
	http.SetCookie(w, &http.Cookie{Name: "sf_" + role, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 3 * 86400})
}

//go:embed console.html
var consoleHTML []byte

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.allowed(r) {
		http.Error(w, "本机演示仅允许 localhost 同源访问。", 403)
		return
	}
	path := r.URL.Path
	if path == "/local-demo/start" {
		if r.Method != "GET" || !security.Equal(r.URL.Query().Get("key"), s.Config.LaunchKey) {
			http.Error(w, "请使用本机启动脚本生成的专属入口。", 403)
			return
		}
		token, e := s.login(r.Context(), "admin", "local-admin@example.invalid")
		if e != nil {
			http.Error(w, "演示登录暂未完成，请稍后重试。", 503)
			return
		}
		sessionCookie(w, "admin", token)
		http.Redirect(w, r, "/local-demo", http.StatusSeeOther)
		return
	}
	if path == "/local-demo" || strings.HasPrefix(path, "/api/local-demo/") {
		if !s.admin(r) {
			http.Error(w, "请先从本机专属入口或管理后台登录，再打开演示控制台。", 401)
			return
		}
		if path == "/local-demo" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(consoleHTML)
			return
		}
		s.demoAPI(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || path == "/healthz" || path == "/livez" {
		s.API.ServeHTTP(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	file := filepath.Join(s.Config.Dist, filepath.FromSlash(strings.TrimPrefix(filepath.Clean("/"+path), "/")))
	if st, e := os.Stat(file); e == nil && !st.IsDir() && filepath.Ext(file) != ".html" {
		http.ServeFile(w, r, file)
		return
	}
	if filepath.Ext(path) != "" && filepath.Ext(path) != ".html" {
		http.NotFound(w, r)
		return
	}
	b, e := os.ReadFile(filepath.Join(s.Config.Dist, "index.html"))
	if e != nil {
		http.Error(w, "请先运行 npm run build。", 503)
		return
	}
	banner := `<style>body{padding-top:36px!important}#local-demo-banner{position:fixed;inset:0 0 auto;z-index:2147483647;background:#172554;color:#fff;font:12px/36px system-ui;text-align:center;height:36px}#local-demo-banner a{color:#93c5fd;margin-left:16px}</style><div id="local-demo-banner">本机演示 · 支付、邮件、供货、域名均为模拟数据<a href="/local-demo">演示控制台 / 本地收件箱</a></div>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, strings.Replace(string(b), "<body>", "<body>"+banner, 1))
}

func (s *Server) demoAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/local-demo/status" && r.Method == "GET" {
		s.State.Lock()
		letters := append([]Letter{}, s.State.Letters...)
		paymentStates := map[string]string{}
		for id, state := range s.State.Payments {
			paymentStates[id] = state
		}
		s.State.Unlock()
		rows, e := s.Store.DB.Query(r.Context(), `SELECT id,name,email,slug FROM partners ORDER BY created_at,id`)
		if e != nil {
			writeJSON(w, 500, map[string]string{"error": "代理读取失败"})
			return
		}
		partners := []map[string]string{}
		for rows.Next() {
			var id, name, email, slug string
			if rows.Scan(&id, &name, &email, &slug) != nil {
				rows.Close()
				writeJSON(w, 500, map[string]string{"error": "代理读取失败"})
				return
			}
			partners = append(partners, map[string]string{"id": id, "name": name, "email": email, "slug": slug})
		}
		rows.Close()
		writeJSON(w, 200, map[string]any{"mode": "local-demo", "externalServices": false, "payments": paymentStates, "letters": letters, "partners": partners, "totp": security.TOTP(s.Config.TOTP, time.Now().Unix()/30)})
		return
	}
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Action string `json:"action"`
		Order  string `json:"order"`
		Email  string `json:"email"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "请求无效"})
		return
	}
	switch r.URL.Path {
	case "/api/local-demo/payment":
		s.State.Lock()
		_, ok := s.State.Payments[in.Order]
		if ok {
			s.State.Payments[in.Order] = "paid"
		}
		e := s.State.save()
		s.State.Unlock()
		if !ok || e != nil {
			writeJSON(w, 409, map[string]string{"error": "请先在原业务页面创建本地付款单。"})
			return
		}
	case "/api/local-demo/partner-login":
		token, e := s.login(r.Context(), "partner", in.Email)
		if e != nil {
			writeJSON(w, 409, map[string]string{"error": "演示代理登录未完成，请检查账号或稍后重试。"})
			return
		}
		sessionCookie(w, "partner", token)
	case "/api/local-demo/transition":
		// Email delivery orders and payment test orders use the local payment adapter.
		p, e := payments.Find(r.Context(), s.Store.DB, in.Order, false)
		if e == nil {
			if in.Action != "pay" {
				writeJSON(w, 400, map[string]string{"error": "自动发货订单只需模拟付款；后续自动交付。"})
				return
			}
			o, e := orders.Get(r.Context(), s.Store.DB, in.Order, nil, false)
			if e != nil {
				writeJSON(w, 404, map[string]string{"error": "订单不存在"})
				return
			}
			s.State.Lock()
			s.State.Payments[in.Order] = "paid"
			e = s.State.save()
			s.State.Unlock()
			if e == nil {
				e = s.Payments.Apply(r.Context(), p, payments.Result{OrderID: in.Order, Status: "paid", TradeNo: "LOCAL-DEMO-" + in.Order, Amount: o.Price + p.Fee})
			}
			if e != nil {
				writeJSON(w, 409, map[string]string{"error": "模拟付款同步失败"})
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		// Creation snapshots the upstream intent; Sync creates the simulated cashier
		// through the original service before applying a channel result.
		if e := s.Recharge.Sync(r.Context(), in.Order, false); e != nil {
			writeJSON(w, 409, map[string]string{"error": "演示供货订单准备失败"})
			return
		}
		if e := s.Supplier.Transition(in.Order, in.Action); e != nil {
			writeJSON(w, 409, map[string]string{"error": e.Error()})
			return
		}
		if e := s.Recharge.Sync(r.Context(), in.Order, true); e != nil {
			writeJSON(w, 409, map[string]string{"error": "模拟状态已保存，订单同步尚未完成，请刷新重试。"})
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
