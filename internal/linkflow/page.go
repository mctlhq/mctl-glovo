package linkflow

import (
	"html/template"
	"time"
)

// The pages carry no script and no style: the CSP forbids both.
var pageTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link a Glovo account</title>
</head>
<body>
<main>
<h1>Link your Glovo account</h1>
<p>Consent text version <code>{{.Version}}</code>.</p>
<ul>
{{range .Text}}<li>{{.}}</li>
{{end}}</ul>
{{if .Code}}
<h2>Your one-time link code</h2>
<p>This code is shown once. It expires at {{.Expires}} and only the newest code works.</p>
<p><code>{{.Code}}</code></p>
<p>Run <code>glovo-link</code> on the machine that has your Glovo session and paste the code when it asks. Never pass the code as a command-line argument.</p>
<p>The helper shows the MCTL account the code belongs to. If it is not yours, answer no.</p>
{{else}}
<h2>How linking works</h2>
<ol>
<li>Record your consent to this text in MCTL.</li>
<li>Request a one-time code from <code>POST /link/code</code> while signed in to MCTL.</li>
<li>Run <code>glovo-link</code> and paste the code when it asks.</li>
</ol>
{{end}}
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
