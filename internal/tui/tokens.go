package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// token form labels → scope
var tokenScopeFields = []struct{ label, scope string }{
	{"Code", store.ScopeCode},
	{"Issues & pull requests", store.ScopeIssues},
	{"CI", store.ScopeCI},
	{"Packages", store.ScopePackages},
}

const (
	presetCustom = "custom"
	presetFull   = "full access"
)

// tokenPresets: name → scopes (presetFull is handled separately)
var tokenPresets = []struct{ name, scopes string }{
	{presetCustom, ""},
	{presetFull, "*"},
	{"read only", "code:read issues:read ci:read packages:read"},
	{"git pull & push", "code:write"},
	{"publish packages", "packages:write"},
}

var tokenExpiries = []string{"7 days", "30 days", "90 days", "1 year", "never"}

// newTokenForm creates an API token: a preset or individual scopes, an
// optional list of repositories, and a lifetime.
func newTokenForm(ctx *Ctx) *FormPage {
	f, c, u := ctx.F, ctx.Ctx, ctx.User
	var presetNames []string
	for _, p := range tokenPresets {
		presetNames = append(presetNames, p.name)
	}
	levels := []string{"none", "read", "write"}
	toCustom := func(fp *FormPage, _ string) { fp.Set("Preset", presetCustom) }
	fields := []*Field{
		{Label: "Name", Placeholder: "laptop-cli", Hint: "what it's for"},
		{Label: "Preset", Options: presetNames, Value: presetCustom, Hint: "←/→ fills in the scopes below"},
	}
	for _, sf := range tokenScopeFields {
		v := "none"
		if sf.scope == store.ScopeCode {
			v = "read"
		}
		fields = append(fields, &Field{Label: sf.label, Options: levels, Value: v, OnChange: toCustom})
	}
	fields = append(fields, &Field{Label: "Admin", Checkbox: true, Placeholder: "settings: create/delete repositories, collaborators, secrets, mirrors, protection", OnChange: toCustom})
	if u.IsAdmin {
		fields = append(fields, &Field{Label: "Site admin", Checkbox: true, Placeholder: "act as site admin: every repository, runners", OnChange: toCustom})
	}
	fields = append(fields,
		&Field{Label: "Repositories", Placeholder: "all — or e.g. " + u.Name + "/app, " + u.Name + "/*", Hint: "limit the token to these"},
		&Field{Label: "Expires", Options: tokenExpiries, Value: "90 days"},
	)
	// presets fill in the scope fields
	fields[1].OnChange = func(fp *FormPage, name string) {
		for _, p := range tokenPresets {
			if p.name != name || name == presetCustom {
				continue
			}
			g, _ := store.ParseScopes(p.scopes)
			for _, sf := range tokenScopeFields {
				lvl := "none"
				if g.Full {
					lvl = "write"
				} else if a := g.Scopes[sf.scope]; a != store.AccessNone {
					lvl = a.String()
				}
				fp.Set(sf.label, lvl)
			}
			fp.Set("Admin", boolStr(g.Full))
			fp.Set("Site admin", boolStr(g.Full && u.IsAdmin))
			if g.Full {
				fp.Set("Repositories", "")
			}
		}
	}
	return NewForm(ctx, "New API token", fields, func(v map[string]string) (tea.Cmd, error) {
		scopes := "*"
		if v["Preset"] != presetFull {
			var parts []string
			for _, sf := range tokenScopeFields {
				if lvl := v[sf.label]; lvl != "none" {
					parts = append(parts, sf.scope+":"+lvl)
				}
			}
			if v["Admin"] == "true" {
				parts = append(parts, store.ScopeAdmin)
			}
			if v["Site admin"] == "true" {
				parts = append(parts, store.ScopeSite)
			}
			scopes = strings.Join(parts, " ")
		}
		g, err := forge.GrantFrom(scopes, v["Repositories"])
		if err != nil {
			return nil, err
		}
		exp, err := forge.ParseExpiry(v["Expires"], time.Now())
		if err != nil {
			return nil, err
		}
		tok, err := f.CreateToken(c, u, forge.TokenSpec{Name: v["Name"], Grant: g, Expires: exp})
		if err != nil {
			return nil, err
		}
		st := ctx.St
		what := g.Summary()
		if !exp.IsZero() {
			what += " · expires " + exp.Local().Format("2006-01-02 15:04")
		} else {
			what += " · never expires"
		}
		return replace(NewTextPage(ctx, "Token created", st.Title.Render("Your new API token")+"\n\n  "+st.Accent.Render(tok)+"\n\n  "+st.Subtle.Render(what)+"\n\n"+
			st.Subtle.Render("Copy it now — it will not be shown again.\nUse it as: curl -H 'Authorization: Bearer <token>' "+f.Cfg.HTTP.PublicURL+"/api/v1/user\nor as the password for git over HTTPS."))), nil
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// tokenRowCols: the account tab's columns for a token.
func tokenRowCols(st *Styles, tk store.AccessToken) []string {
	used := "never used"
	if tk.LastUsed.Unix() > 0 {
		used = "used " + ago(tk.LastUsed)
	}
	exp := forge.ExpiryText(tk, time.Now())
	expStyled := st.Faint.Render(exp)
	if tk.Expired() {
		expStyled = st.Error.Render(exp)
	} else if !tk.ExpiresAt.IsZero() && time.Until(tk.ExpiresAt) < 7*24*time.Hour {
		expStyled = st.StatusWait.Render(exp)
	}
	return []string{st.Label.Render("token"), tk.Name, st.Subtle.Render(tk.Grant.Summary()) + st.Faint.Render(" · ") + expStyled + st.Faint.Render(" · "+used), st.Faint.Render(ago(tk.CreatedAt))}
}
