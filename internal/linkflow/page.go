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
<ol>
<li>Record your consent to this text in MCTL.</li>
<li>Run <code>glovo-link</code> on the machine that has your Glovo session and sign in to MCTL once when it asks.</li>
<li>The helper requests a one-time code for your account, shows which MCTL account it belongs to and asks you to confirm. There is nothing to paste.</li>
</ol>
{{end}}
</section>
</main>
</body>
</html>
`))

type pageData struct {
	Version string
	Text    []string
	Code    string
	Expires string
}

func newPage(code string, expires time.Time) pageData {
	d := pageData{Version: ConsentVersion, Text: ConsentText, Code: code}
	if code != "" {
		d.Expires = expires.UTC().Format(time.RFC3339)
	}
	return d
}
