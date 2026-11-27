package linkflow

import (
	"html/template"
	"time"
)

// The pages carry no script and no style attribute. Their one inline <style>
// (pageCSS) is admitted by its sha256 in the CSP (pageCSP), the same way the
// landing page does it; everything else stays denied.
var pageTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link a Glovo account</title>
<style>` + pageCSS + `</style>
</head>
<body>
<header class="band"><div class="wrap">
<h1>Link your Glovo account</h1>
<p>Consent text version <code>{{.Version}}</code>.</p>
</div></header>
<main class="wrap">
<section>
<ul>
{{range .Text}}<li>{{.}}</li>
{{end}}</ul>
</section>
<section>
{{if .Code}}
<h2>Your one-time link code</h2>
<p>This code is shown once. It expires at {{.Expires}} and only the newest code works.</p>
<p><code class="code">{{.Code}}</code></p>
<p>Normally you do not need this page: <code>glovo-link</code> requests its own code after one MCTL login. Never pass a code as a command-line argument.</p>
<p>The helper shows the MCTL account the code belongs to. If it is not yours, answer no.</p>
{{else}}
<h2>How linking works</h2>
<p>Linking has two parts: a Glovo session file made on your own computer, and the helper <code>glovo-link</code> that uploads it. Your Glovo account must be able to sign in with a password or an email code. Accounts that only use Google or Facebook sign-in cannot be linked yet.</p>
<ol>
<li>Open <code>{{.AccessURL}}</code>, sign in with the Google account you connect your assistant with, record your consent to this text and request access. Wait until the request is approved before you continue.</li>
<li>Create the session file with glovo-skill ({{.SkillURL}}, a third-party tool, MIT licence). It needs Node.js 18 or newer and Google Chrome. Run <code>git clone {{.SkillURL}}.git</code>, then in that folder <code>npm ci</code>, <code>npm run build</code> and <code>node scripts/login.mjs</code>. In the window that opens, sign in to Glovo with your password or an email code, not with the Google button: Google refuses to sign in inside that window. The file is saved to <code>~/.glovo/session.json</code>.</li>
<li>Install the helper (it needs Go): <code>go install github.com/mctlhq/mctl-glovo/cmd/glovo-link&#64;{{.HelperVersion}}</code></li>
<li>Within about 20 minutes of step 2, run <code>glovo-link -client-id {{.ClientID}}</code> and sign in to MCTL once when it asks. A Glovo login is only valid for about 20 minutes and an older file is refused.</li>
<li>The helper requests a one-time code for your account, shows which MCTL account it belongs to and asks you to confirm. There is nothing to paste.</li>
</ol>
<p>To remove your stored session later, run <code>glovo-link -unlink -client-id {{.ClientID}}</code>.</p>
{{end}}
</section>
</main>
</body>
</html>
`))

type pageData struct {
	Version       string
	Text          []string
	Code          string
	Expires       string
	HelperVersion string
	ClientID      string
	SkillURL      string
	AccessURL     string
}

func newPage(code string, expires time.Time) pageData {
	d := pageData{Version: ConsentVersion, Text: ConsentText, Code: code,
		HelperVersion: HelperVersion, ClientID: HelperClientID, SkillURL: SkillURL, AccessURL: AccessURL}
	if code != "" {
		d.Expires = expires.UTC().Format(time.RFC3339)
	}
	return d
}
