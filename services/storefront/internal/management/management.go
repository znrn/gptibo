package management

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Service struct {
	PlatformLinksOnly bool
	DomainsEnabled    func() bool
	Domains           *domains.Service
	Origin            string
	Store             *store.Store
	Auth              *auth.Service
	VerifySupplier    func(context.Context, string) error
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func validMoney(n int64) bool { return n >= 1 && n <= 10000000 }
func validText(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}
func conflict() error { return fault.New(409, "数据已更新或已存在，请刷新后重试。") }

func (s Service) write(ctx context.Context, role, token string, fn func(pgx.Tx, auth.Principal) error) error {
	return s.writeTransaction(ctx, role, token, false, fn)
}
func (s Service) writeHierarchy(ctx context.Context, role, token string, fn func(pgx.Tx, auth.Principal) error) error {
	return s.writeTransaction(ctx, role, token, true, fn)
}
func (s Service) writeTransaction(ctx context.Context, role, token string, exclusive bool, fn func(pgx.Tx, auth.Principal) error) error {
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Acquire before auth row locks so parent/child edits cannot deadlock with orders.
	lockSQL := "SELECT pg_advisory_xact_lock_shared(903495)"
	if exclusive {
		lockSQL = "SELECT pg_advisory_xact_lock(903495)"
	}
	if _, e = tx.Exec(ctx, lockSQL); e != nil {
		return e
	}
	p, e := s.Auth.Require(ctx, tx, role, token)
	if e != nil {
		return e
	}
	if e = fn(tx, p); e != nil {
		var pgerr *pgconn.PgError
		if errors.As(e, &pgerr) && pgerr.Code == "23505" {
			return conflict()
		}
		return e
	}
	return tx.Commit(ctx)
}
func audit(ctx context.Context, tx pgx.Tx, p auth.Principal, action, target string, detail any) error {
	b, e := json.Marshal(detail)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "INSERT INTO audit_events(id,actor_role,actor_id,action,target,detail) VALUES($1,$2,$3,$4,$5,$6)", security.ID(), p.Role, p.Subject, action, target, b)
	return e
}

// Table names are an internal allowlist; snapshots never include authentication secrets.
func snapshot(ctx context.Context, tx pgx.Tx, table, id string) (map[string]any, error) {
	if table != "catalog_plans" && table != "partners" {
		return nil, errors.New("unsupported audit snapshot")
	}
	var value map[string]any
	err := tx.QueryRow(ctx, "SELECT to_jsonb(r) FROM "+table+" r WHERE id=$1 FOR UPDATE", id).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return value, err
}

type queryable interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func rows(ctx context.Context, q queryable, sql string, args ...any) ([]map[string]any, error) {
	r, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []map[string]any{}
	for r.Next() {
		values, e := r.Values()
		if e != nil {
			return nil, e
		}
		item := map[string]any{}
		for i, f := range r.FieldDescriptions() {
			item[f.Name] = values[i]
		}
		out = append(out, item)
	}
	return out, r.Err()
}

const planColumns = `supports_warranty,id,product_id,name,price_cents,enabled::int,partner_enabled::int,renewal_only::int,purchase_note,delivery_instructions,version,default_cost_cents,sort_order,(extract(epoch FROM updated_at)*1000)::bigint AS updated_at`

func (s Service) Plans(ctx context.Context) ([]map[string]any, error) {
	return rows(ctx, s.Store.DB, "SELECT "+planColumns+" FROM catalog_plans ORDER BY sort_order,id")
}

type PlanInput struct {
	SupportsWarranty     *bool   `json:"supportsWarranty"`
	ID                   string  `json:"id"`
	ProductID            string  `json:"productId"`
	Name                 string  `json:"name"`
	PriceCents           int64   `json:"priceCents"`
	DefaultCostCents     *int64  `json:"defaultCostCents"`
	Enabled              *bool   `json:"enabled"`
	PartnerEnabled       *bool   `json:"partnerEnabled"`
	RenewalOnly          *bool   `json:"renewalOnly"`
	Note                 string  `json:"note"`
	PurchaseNote         *string `json:"purchaseNote"`
	DeliveryInstructions *string `json:"deliveryInstructions"`
	Version              *int64  `json:"version"`
}

func (s Service) SavePlan(ctx context.Context, token, id string, in PlanInput, create bool) error {
	if !validMoney(in.PriceCents) || in.Enabled == nil || strings.TrimSpace(in.Note) == "" || !validText(in.Note, 500) {
		return fault.New(400, "请填写有效价格、售卖状态和操作说明。")
	}
	if in.DefaultCostCents != nil && (!validMoney(*in.DefaultCostCents) || *in.DefaultCostCents > in.PriceCents) {
		return fault.New(400, "默认供货价不能高于售价。")
	}
	for _, v := range []*string{in.PurchaseNote, in.DeliveryInstructions} {
		if v != nil && !validText(*v, 2000) {
			return fault.New(400, "商品说明最多 2000 字。")
		}
	}
	if create {
		id = in.ID
		if !identifier.MatchString(id) || !identifier.MatchString(in.ProductID) || strings.TrimSpace(in.Name) == "" || !validText(in.Name, 100) || in.DefaultCostCents == nil {
			return fault.New(400, "请填写商品编号、名称和默认供货价。")
		}
	} else if in.Version == nil || *in.Version < 0 {
		return fault.New(400, "请提供当前版本。")
	}
	return s.writeHierarchy(ctx, "admin", token, func(tx pgx.Tx, p auth.Principal) error {
		before, e := snapshot(ctx, tx, "catalog_plans", id)
		if e != nil {
			return e
		}
		if create {
			_, e := tx.Exec(ctx, `INSERT INTO catalog_plans(id,product_id,name,price_cents,default_cost_cents,enabled,partner_enabled,renewal_only,purchase_note,delivery_instructions,supports_warranty)
 VALUES($1,$2,$3,$4,$5,$6,COALESCE($7,false),COALESCE($8,false),COALESCE($9,''),COALESCE($10,''),COALESCE($11,true))`, id, in.ProductID, strings.TrimSpace(in.Name), in.PriceCents, in.DefaultCostCents, in.Enabled, in.PartnerEnabled, in.RenewalOnly, in.PurchaseNote, in.DeliveryInstructions, in.SupportsWarranty)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO partner_prices(partner_id,plan,price_cents,cost_cents) SELECT id,$1,$2,$3 FROM partners", id, in.PriceCents, in.DefaultCostCents); e != nil {
				return e
			}
		} else {
			var oldCost *int64
			e := tx.QueryRow(ctx, "SELECT default_cost_cents FROM catalog_plans WHERE id=$1 FOR UPDATE", id).Scan(&oldCost)
			if errors.Is(e, pgx.ErrNoRows) {
				return fault.New(404, "套餐不存在。")
			}
			if e != nil {
				return e
			}
			if in.DefaultCostCents == nil && oldCost != nil && *oldCost > in.PriceCents {
				return fault.New(400, "售价不能低于默认供货价。")
			}
			tag, e := tx.Exec(ctx, `UPDATE catalog_plans SET price_cents=$1,enabled=$2,default_cost_cents=COALESCE($3,default_cost_cents),
 partner_enabled=COALESCE($4,partner_enabled),renewal_only=COALESCE($5,renewal_only),purchase_note=COALESCE($6,purchase_note),
 supports_warranty=COALESCE($10,supports_warranty),delivery_instructions=COALESCE($7,delivery_instructions),version=version+1,updated_at=now() WHERE id=$8 AND version=$9`, in.PriceCents, in.Enabled, in.DefaultCostCents, in.PartnerEnabled, in.RenewalOnly, in.PurchaseNote, in.DeliveryInstructions, id, in.Version, in.SupportsWarranty)
			if e != nil {
				return e
			}
			if tag.RowsAffected() != 1 {
				return conflict()
			}
		}
		after, e := snapshot(ctx, tx, "catalog_plans", id)
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, "plan-save", id, map[string]any{"price_cents": in.PriceCents, "enabled": *in.Enabled, "note": strings.TrimSpace(in.Note), "created": create, "before": before, "after": after})
	})
}

func (s Service) CreatePartner(ctx context.Context, token, email string) (string, string, error) {
	email, e := auth.Email(email)
	if e != nil {
		return "", "", e
	}
	var id, slug string
	e = s.writeHierarchy(ctx, "admin", token, func(tx pgx.Tx, p auth.Principal) error {
		var err error
		id, slug, err = ProvisionPartner(ctx, tx, p, email, email)
		return err
	})
	return id, slug, e
}

// Called within the authenticated admin or verified Telegram application transaction.
func ProvisionPartner(ctx context.Context, tx pgx.Tx, actor auth.Principal, email, name string) (string, string, error) {
	if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(903495)"); e != nil {
		return "", "", e
	}
	var exists bool
	if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partners WHERE email=$1)`, email).Scan(&exists); e != nil {
		return "", "", e
	}
	if exists {
		return "", "", fault.New(409, "该邮箱已有代理账号，请使用申请邮箱登录工作台，或联系管理员。")
	}
	id, slug := security.ID(), ""
	created := false
	for range 8 {
		slug = strings.ToUpper(security.Token()[:6])
		tag, e := tx.Exec(ctx, `INSERT INTO partners(id,slug,name,email,active) SELECT $1,$2,$3,$4,true WHERE NOT EXISTS(SELECT 1 FROM partner_stores WHERE slug=$2) ON CONFLICT(slug) DO NOTHING`, id, slug, name, email)
		if e != nil {
			return "", "", e
		}
		if tag.RowsAffected() == 1 {
			created = true
			break
		}
	}
	if !created {
		return "", "", fault.New(503, "推广码生成暂时失败，请重试。")
	}
	if _, e := tx.Exec(ctx, `INSERT INTO partner_prices(partner_id,plan,price_cents,cost_cents) SELECT $1,id,price_cents,COALESCE(default_cost_cents,price_cents) FROM catalog_plans`, id); e != nil {
		return "", "", e
	}
	if e := audit(ctx, tx, actor, "partner-create", id, map[string]any{"slug": slug}); e != nil {
		return "", "", e
	}
	return id, slug, nil
}
func (s Service) Partners(ctx context.Context, page int, query string) ([]map[string]any, int, error) {
	if page < 1 || page > 10000 || !validText(query, 100) {
		return nil, 0, fault.New(400, "分页或查询条件无效。")
	}
	where := ` WHERE strpos(lower(p.name),lower($1))>0 OR strpos(lower(COALESCE(p.email,'')),lower($1))>0 OR strpos(lower(p.slug),lower($1))>0`
	var total int
	e := s.Store.DB.QueryRow(ctx, "SELECT count(*) FROM partners p"+where, query).Scan(&total)
	if e != nil {
		return nil, 0, e
	}
	result, e := rows(ctx, s.Store.DB, `SELECT p.id,p.name,p.email,p.slug,p.active::int,p.version,p.parent_id,p.can_recruit,COALESCE(sc.wechat,'') wechat,COALESCE(sc.qq,'') qq,(extract(epoch FROM p.created_at)*1000)::bigint AS created_at FROM partners p LEFT JOIN support_contacts sc ON sc.owner=p.id`+where+` ORDER BY p.created_at DESC,p.id LIMIT 20 OFFSET $2`, query, (page-1)*20)
	return result, total, e
}

type PartnerInput struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Active  *bool  `json:"active"`
	Version *int64 `json:"version"`
}

func (s Service) SavePartner(ctx context.Context, token, id string, in PartnerInput) error {
	email, e := auth.Email(in.Email)
	if e != nil {
		return e
	}
	if strings.TrimSpace(in.Name) == "" || !validText(in.Name, 100) || in.Active == nil || in.Version == nil || *in.Version < 0 {
		return fault.New(400, "代理资料或版本无效。")
	}
	return s.writeHierarchy(ctx, "admin", token, func(tx pgx.Tx, p auth.Principal) error {
		before, e := snapshot(ctx, tx, "partners", id)
		if e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `UPDATE partners SET name=$1,email=$2,active=$3,auth_version=auth_version+CASE WHEN email IS DISTINCT FROM $2 OR active<>$3 THEN 1 ELSE 0 END,version=version+1 WHERE id=$4 AND version=$5`, strings.TrimSpace(in.Name), email, *in.Active, id, *in.Version)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return conflict()
		}
		after, e := snapshot(ctx, tx, "partners", id)
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, "partner-update", id, map[string]any{"active": *in.Active, "before": before, "after": after})
	})
}
func (s Service) PartnerPlans(ctx context.Context, id string) ([]map[string]any, error) {
	return rows(ctx, s.Store.DB, `SELECT c.id,c.product_id,c.name,c.renewal_only::int,(c.enabled AND c.partner_enabled AND p.enabled)::int AS enabled,p.price_cents,p.cost_cents,p.version
 FROM partner_prices p JOIN catalog_plans c ON c.id=p.plan WHERE p.partner_id=$1 ORDER BY c.sort_order,c.id`, id)
}

type PriceInput struct {
	PriceCents int64  `json:"priceCents"`
	CostCents  int64  `json:"costCents"`
	Version    *int64 `json:"version"`
}

func (s Service) Price(ctx context.Context, role, token, partnerID, plan string, in PriceInput) error {
	if in.Version == nil || *in.Version < 0 {
		return fault.New(400, "价格版本无效。")
	}
	value := in.PriceCents
	if role == "admin" {
		value = in.CostCents
	}
	if !validMoney(value) {
		return fault.New(400, "价格应在 0.01–100000 元之间。")
	}
	return s.writeHierarchy(ctx, role, token, func(tx pgx.Tx, p auth.Principal) error {
		if role == "partner" {
			partnerID = p.Subject
		}
		var price, cost, version int64
		e := tx.QueryRow(ctx, "SELECT price_cents,cost_cents,version FROM partner_prices WHERE partner_id=$1 AND plan=$2 FOR UPDATE", partnerID, plan).Scan(&price, &cost, &version)
		if errors.Is(e, pgx.ErrNoRows) {
			return fault.New(404, "该套餐尚未开通。")
		}
		if e != nil {
			return e
		}
		if version != *in.Version {
			return conflict()
		}
		if role == "admin" {
			var invalid bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partners child JOIN partner_prices cp ON cp.partner_id=child.id WHERE child.parent_id=$1 AND cp.plan=$2 AND cp.cost_cents<$3) OR EXISTS(SELECT 1 FROM partners child JOIN partner_prices pp ON pp.partner_id=child.parent_id WHERE child.id=$1 AND pp.plan=$2 AND pp.cost_cents>$3)`, partnerID, plan, value).Scan(&invalid); e != nil {
				return e
			}
			if invalid {
				return fault.New(409, "供货价与上下级价格冲突，请先协调调整。")
			}
			cost = value
		} else {
			price = value
		}
		if cost > price {
			return fault.New(400, "售价不能低于供货价。")
		}
		if role == "admin" {
			if e = checkAccountFloor(ctx, tx, partnerID, plan, cost); e != nil {
				return e
			}
		} else {
			if e = checkStoreFloor(ctx, tx, partnerID, partnerID, plan, price, cost); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO store_prices(store_id,plan,price_cents,version) VALUES($2,$3,$1,1) ON CONFLICT(store_id,plan) DO UPDATE SET price_cents=$1,version=store_prices.version+1`, price, partnerID, plan); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "UPDATE partner_prices SET price_cents=$1,cost_cents=$2,child_cost_cents=greatest(child_cost_cents,$2),version=version+1 WHERE partner_id=$3 AND plan=$4", price, cost, partnerID, plan)
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, "partner-price", partnerID, map[string]any{"plan": plan, "price_cents": price, "cost_cents": cost})
	})
}
