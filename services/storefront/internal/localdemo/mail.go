package localdemo

import (
	"context"
	"encoding/json"
	"errors"
	"gptibo/storefront/internal/mailer"
	"net/mail"
	"time"
)

type Mailbox struct{ State *State }

func (m Mailbox) put(l Letter) error {
	m.State.Lock()
	defer m.State.Unlock()
	for _, old := range m.State.Letters {
		if old.ID == l.ID {
			return nil
		}
	}
	l.Time = time.Now().Format(time.RFC3339)
	m.State.Letters = append(m.State.Letters, l)
	if len(m.State.Letters) > 500 {
		m.State.Letters = m.State.Letters[len(m.State.Letters)-500:]
	}
	return m.State.save()
}
func (m Mailbox) SendCode(_ context.Context, role, to, code, id string) error {
	return m.put(Letter{ID: id, To: to, Subject: "本机演示 · " + role + " 登录验证码", Text: "验证码：" + code + "（仅用于本机演示）", Code: code})
}
func (m Mailbox) SendInvitation(_ context.Context, to, origin, id string) error {
	return m.put(Letter{ID: id, To: to, Subject: "本机演示 · 代理邀请", Text: "请访问 " + origin + "/partner，使用此邮箱登录。验证码在本地收件箱查看。"})
}
func (m Mailbox) Freeze(to string, msg mailer.Message) (string, error) {
	if _, e := mail.ParseAddress(to); e != nil {
		return "", e
	}
	b, e := json.Marshal(mailer.Frozen{From: "GPTibo Local <demo@example.invalid>", To: []string{to}, Message: msg})
	return string(b), e
}
func (m Mailbox) SendFrozen(_ context.Context, body, key string) (string, error) {
	var f mailer.Frozen
	if json.Unmarshal([]byte(body), &f) != nil || len(f.To) != 1 || key == "" {
		return "", errors.New("invalid local mail")
	}
	text := f.Text
	if text == "" {
		text = "此邮件只有 HTML 正文。主题：" + f.Subject
	}
	e := m.put(Letter{ID: key, To: f.To[0], Subject: f.Subject, Text: text})
	return "LOCAL-MAIL-" + key, e
}
func (m Mailbox) Code(id string) string {
	m.State.Lock()
	defer m.State.Unlock()
	for _, l := range m.State.Letters {
		if l.ID == id {
			return l.Code
		}
	}
	return ""
}
