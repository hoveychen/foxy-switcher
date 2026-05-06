package httpserver

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// RegisterWebRoutes adds the HTML surface (admin Web UI) to the given
// mux. Called by main on its root mux so the web pages share the same
// listener as the frontend httpapi and the agent surface. In agent mode
// the daemon doesn't call this.
func (s *Server) RegisterWebRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	// "GET /{$}" matches only the exact root, so it doesn't conflict with
	// the "/agent/v1/" prefix mount or any other prefix the catch-all root
	// handler eventually picks up.
	mux.HandleFunc("GET /{$}", s.requireSession(s.handleHome))
	mux.HandleFunc("GET /pair", s.requireSession(s.handlePairForm))
	mux.HandleFunc("POST /pair", s.requireSession(s.handlePairSubmit))
	mux.HandleFunc("GET /devices", s.requireSession(s.handleDevicesList))
	mux.HandleFunc("POST /devices/revoke", s.requireSession(s.handleDevicesRevoke))
	mux.HandleFunc("GET /password", s.requireSession(s.handlePasswordForm))
	mux.HandleFunc("POST /password", s.requireSession(s.handlePasswordSubmit))
}

// --- /setup --------------------------------------------------------------

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	ok, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if ok {
		// Password already set — setup is one-shot. Send the user to login.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	renderTemplate(w, "Set vault password", setupTemplate, nil)
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	ok, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pw := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")
	if pw == "" || pw != confirm {
		renderTemplate(w, "Set vault password", setupTemplate,
			map[string]string{"Error": "Passwords must match and be non-empty."})
		return
	}
	hash, err := vaultauth.HashPassword(pw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetPasswordHash(r.Context(), hash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.startSession(r.Context(), w, r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- /login --------------------------------------------------------------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	ok, err := s.st.HasPassword(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if s.hasSession(r) {
		http.Redirect(w, r, redirectAfterLogin(r), http.StatusSeeOther)
		return
	}
	renderTemplate(w, "Sign in", loginTemplate,
		map[string]string{"Next": r.URL.Query().Get("next")})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pw := r.PostFormValue("password")
	hash, err := s.st.PasswordHash(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !vaultauth.VerifyPassword(hash, pw) {
		renderTemplate(w, "Sign in", loginTemplate, map[string]string{
			"Error": "Wrong password.",
			"Next":  r.PostFormValue("next"),
		})
		return
	}
	if err := s.startSession(r.Context(), w, r); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, redirectAfterLogin(r), http.StatusSeeOther)
}

func redirectAfterLogin(r *http.Request) string {
	next := r.URL.Query().Get("next")
	if next == "" {
		next = r.PostFormValue("next")
	}
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// --- /logout -------------------------------------------------------------

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.endSession(r.Context(), w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- / -------------------------------------------------------------------

// handleHome is signed-in users' landing page. When the React account
// panel was bundled into this binary (webapp.Available), we send them
// straight there so the embedded UI is the canonical experience and
// the bare server-rendered home stays a fallback. Otherwise the
// fallback summary still renders, so a deployment without `pnpm build`
// run is debuggable.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if s.HasWebApp {
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	devs, err := s.st.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	renderTemplate(w, "Vault", homeTemplate, map[string]any{
		"DeviceCount": len(devs),
	})
}

// --- /pair ---------------------------------------------------------------

func (s *Server) handlePairForm(w http.ResponseWriter, r *http.Request) {
	rawCode := r.URL.Query().Get("code")
	if rawCode == "" {
		renderTemplate(w, "Pair device", pairFormTemplate, nil)
		return
	}
	code := vaultauth.NormaliseUserCode(rawCode)
	p, err := s.st.FindPairingByCode(r.Context(), code)
	if err != nil {
		if notFoundIs(err) {
			renderTemplate(w, "Pair device", pairFormTemplate,
				map[string]string{"Code": code, "Error": "Code expired or unknown."})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if p.Status != store.PairingPending {
		renderTemplate(w, "Pair device", pairFormTemplate,
			map[string]string{"Code": code, "Error": "This code has already been answered."})
		return
	}
	renderTemplate(w, "Pair device", pairConfirmTemplate, map[string]string{
		"Code":       code,
		"DeviceName": p.DeviceName,
	})
}

func (s *Server) handlePairSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	code := vaultauth.NormaliseUserCode(r.PostFormValue("code"))
	action := r.PostFormValue("action")
	if code == "" {
		renderTemplate(w, "Pair device", pairFormTemplate,
			map[string]string{"Error": "Enter a code."})
		return
	}

	switch action {
	case "deny":
		if err := s.st.DenyPairing(r.Context(), code); err != nil && !notFoundIs(err) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		renderTemplate(w, "Pair device", pairResultTemplate,
			map[string]string{"Result": "Pairing denied."})
	case "approve":
		// Generate the device + token here. The pair-poll handler will
		// promote them into the devices table when the agent next polls.
		token := vaultauth.NewToken()
		deviceID := vaultauth.NewID()
		if err := s.st.ApprovePairing(r.Context(), code, deviceID, token); err != nil {
			if notFoundIs(err) {
				renderTemplate(w, "Pair device", pairFormTemplate,
					map[string]string{"Code": code, "Error": "Code expired or already used."})
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		renderTemplate(w, "Pair device", pairResultTemplate,
			map[string]string{"Result": "Pairing approved. The agent should pick up its token within a few seconds."})
	default:
		writeError(w, http.StatusBadRequest, errors.New("action must be approve or deny"))
	}
}

// --- /devices ------------------------------------------------------------

func (s *Server) handleDevicesList(w http.ResponseWriter, r *http.Request) {
	devs, err := s.st.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type row struct {
		ID         string
		Name       string
		Hostname   string
		OS         string
		OSVersion  string
		Arch       string
		Model      string
		AppVersion string
		ClientType string
		CreatedAt  string
		LastSeenAt string
	}
	now := time.Now()
	out := make([]row, 0, len(devs))
	for _, d := range devs {
		out = append(out, row{
			ID:         d.ID,
			Name:       d.Name,
			Hostname:   d.Hostname,
			OS:         d.OS,
			OSVersion:  d.OSVersion,
			Arch:       d.Arch,
			Model:      d.Model,
			AppVersion: d.AppVersion,
			ClientType: d.ClientType,
			CreatedAt:  formatRelative(now, d.CreatedAt),
			LastSeenAt: formatRelative(now, d.LastSeenAt),
		})
	}
	renderTemplate(w, "Devices", devicesTemplate, map[string]any{"Devices": out})
}

func (s *Server) handleDevicesRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PostFormValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id required"))
		return
	}
	if err := s.st.DeleteDevice(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// --- /password -----------------------------------------------------------

func (s *Server) handlePasswordForm(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "Change password", passwordTemplate, nil)
}

func (s *Server) handlePasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	current := r.PostFormValue("current")
	next := r.PostFormValue("next")
	confirm := r.PostFormValue("confirm")
	if next == "" || next != confirm {
		renderTemplate(w, "Change password", passwordTemplate,
			map[string]string{"Error": "New passwords must match and be non-empty."})
		return
	}
	hash, err := s.st.PasswordHash(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !vaultauth.VerifyPassword(hash, current) {
		renderTemplate(w, "Change password", passwordTemplate,
			map[string]string{"Error": "Current password is wrong."})
		return
	}
	newHash, err := vaultauth.HashPassword(next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetPasswordHash(r.Context(), newHash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	renderTemplate(w, "Change password", passwordTemplate,
		map[string]string{"Success": "Password updated."})
}

// --- helpers -------------------------------------------------------------

func formatRelative(now time.Time, ts int64) string {
	if ts == 0 {
		return "never"
	}
	diff := now.Sub(time.UnixMilli(ts))
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	}
}

// renderTemplate is the one shared HTML escape hatch — every page is a
// single Go template parsed against the shared chrome. We don't precompile
// because the page count is small and parse cost is dwarfed by network
// latency on the request path.
func renderTemplate(w http.ResponseWriter, title, body string, data any) {
	tmpl := template.Must(template.New("page").Parse(chromeTemplate))
	tmpl = template.Must(tmpl.Parse(body))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "page", struct {
		Title string
		Data  any
	}{Title: title, Data: data}); err != nil {
		// Template error → 500. We don't surface the error to the user
		// (it might leak template internals) but log it for the operator.
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// chromeTemplate is the wrapping HTML. Each page template defines a
// "content" block that gets inlined here.
const chromeTemplate = `
{{define "page"}}<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} — Foxy Vault</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
         max-width: 32rem; margin: 2rem auto; padding: 0 1rem;
         line-height: 1.5; }
  h1 { margin-top: 0; font-size: 1.4rem; }
  nav { font-size: .9rem; opacity: .8; margin-bottom: 1rem; }
  nav a { margin-right: .8rem; }
  form { display: grid; gap: .6rem; }
  label { display: grid; gap: .2rem; }
  input[type=password], input[type=text] {
    padding: .4rem .5rem; font-size: 1rem;
    border: 1px solid #888; border-radius: .25rem;
    background: transparent; color: inherit;
  }
  button { padding: .4rem .9rem; font-size: 1rem;
           border: 1px solid #888; border-radius: .25rem;
           background: #eee; color: #111; cursor: pointer; }
  @media (prefers-color-scheme: dark) {
    button { background: #333; color: #eee; border-color: #555; }
  }
  .error { color: #c33; }
  .success { color: #393; }
  table { width: 100%; border-collapse: collapse; margin-top: 1rem; }
  th, td { text-align: left; padding: .35rem .5rem;
           border-bottom: 1px solid rgba(128,128,128,.3); }
  .row-actions form { display: inline; }
  code { font-family: ui-monospace, SF Mono, monospace;
         background: rgba(128,128,128,.15); padding: .1rem .35rem;
         border-radius: .2rem; }
</style>
</head><body>
<nav>
  <a href="/">Home</a>
  <a href="/devices">Devices</a>
  <a href="/pair">Pair</a>
  <a href="/password">Password</a>
  <form action="/logout" method="POST" style="display:inline">
    <button type="submit" style="padding:.1rem .5rem;font-size:.85rem">Sign out</button>
  </form>
</nav>
<h1>{{.Title}}</h1>
{{template "content" .Data}}
</body></html>{{end}}`

// --- page templates ------------------------------------------------------

const setupTemplate = `
{{define "content"}}
<p>This is a fresh vault. Set the admin password — you'll use it to approve
agent pairings and manage devices.</p>
<form action="/setup" method="POST">
  <label>Password<input type="password" name="password" autofocus required></label>
  <label>Confirm<input type="password" name="confirm" required></label>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <button type="submit">Set password</button>
</form>
{{end}}`

const loginTemplate = `
{{define "content"}}
<form action="/login" method="POST">
  <label>Password<input type="password" name="password" autofocus required></label>
  <input type="hidden" name="next" value="{{.Next}}">
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <button type="submit">Sign in</button>
</form>
{{end}}`

const homeTemplate = `
{{define "content"}}
<p>Vault is running. {{.DeviceCount}} device(s) paired.</p>
<p>To pair a new device, run the agent's pair command — it'll show you a
short user code. Paste it on the <a href="/pair">Pair page</a>.</p>
<p>Manage paired devices on the <a href="/devices">Devices page</a>.</p>
{{end}}`

const pairFormTemplate = `
{{define "content"}}
<p>Enter the user code your agent printed.</p>
<form action="/pair" method="GET">
  <label>User code<input type="text" name="code" value="{{.Code}}" autofocus
        autocapitalize="characters" placeholder="ABCD-1234" required></label>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <button type="submit">Look up</button>
</form>
{{end}}`

const pairConfirmTemplate = `
{{define "content"}}
<p>An agent calling itself <strong>{{.DeviceName}}</strong> is requesting
to pair with this vault using code <code>{{.Code}}</code>.</p>
<p>If you didn't initiate this from another machine, deny it.</p>
<form action="/pair" method="POST">
  <input type="hidden" name="code" value="{{.Code}}">
  <div>
    <button type="submit" name="action" value="approve">Approve</button>
    <button type="submit" name="action" value="deny">Deny</button>
  </div>
</form>
{{end}}`

const pairResultTemplate = `
{{define "content"}}
<p>{{.Result}}</p>
<p><a href="/devices">Back to devices</a></p>
{{end}}`

const devicesTemplate = `
{{define "content"}}
{{if .Devices}}
<table>
  <thead><tr>
    <th>Name</th>
    <th>OS</th>
    <th>Arch</th>
    <th>Model</th>
    <th>App</th>
    <th>Paired</th>
    <th>Last seen</th>
    <th></th>
  </tr></thead>
  <tbody>
  {{range .Devices}}
    <tr>
      <td>
        {{.Name}}
        {{if and .Hostname (ne .Hostname .Name)}}<br><small style="color:#888">{{.Hostname}}</small>{{end}}
      </td>
      <td>{{if .OS}}{{.OS}}{{if .OSVersion}} {{.OSVersion}}{{end}}{{else}}&mdash;{{end}}</td>
      <td>{{if .Arch}}{{.Arch}}{{else}}&mdash;{{end}}</td>
      <td>{{if .Model}}{{.Model}}{{else}}&mdash;{{end}}</td>
      <td>{{if .AppVersion}}{{.AppVersion}}{{if .ClientType}} ({{.ClientType}}){{end}}{{else}}{{if .ClientType}}({{.ClientType}}){{else}}&mdash;{{end}}{{end}}</td>
      <td>{{.CreatedAt}}</td>
      <td>{{.LastSeenAt}}</td>
      <td class="row-actions">
        <form action="/devices/revoke" method="POST"
              onsubmit="return confirm('Revoke {{.Name}}?')">
          <input type="hidden" name="id" value="{{.ID}}">
          <button type="submit">Revoke</button>
        </form>
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}
<p>No paired devices yet.</p>
{{end}}
<p><a href="/pair">Pair a new device</a></p>
{{end}}`

const passwordTemplate = `
{{define "content"}}
<form action="/password" method="POST">
  <label>Current password<input type="password" name="current" autofocus required></label>
  <label>New password<input type="password" name="next" required></label>
  <label>Confirm<input type="password" name="confirm" required></label>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  {{if .Success}}<p class="success">{{.Success}}</p>{{end}}
  <button type="submit">Update password</button>
</form>
{{end}}`
