package linkflow

import (
	"crypto/sha256"
	"encoding/base64"
)

// pageCSS is the one inline stylesheet of every HTML page this package
// renders. It follows the landing page's visual language (internal/gateway,
// landing_css.go): the same cream and yellow in light, the same dark surface
// under prefers-color-scheme, the same terracotta accent, a system font stack,
// no external font, image or script. The palette values are the pinned MCTL
// design tokens the landing page uses, written out here because this page
// ships a few hundred bytes of rules, not the whole shared sheet.
//
// The stylesheet is admitted by its sha256 in the Content-Security-Policy
// (pageCSP), computed from this very constant at init, so editing the CSS
// cannot leave a stale hash behind. Rules must not contain "{{" (the template
// parser would read it) and the pages must not use style="" attributes: only
// the hashed <style> element is allowed.
const pageCSS = `:root{
  --bg:#fff8e6;--card:#ffffff;--line:#eadcb8;--line-strong:#d6c18a;
  --fg:#15181d;--muted:#3a3f47;--band:#ffc244;--band-fg:#15181d;
  --code-bg:#f4ead0;--accent:#b83d28;--accent-text:#ab301b;--focus:#15181d;
  --shadow:0 1px 2px rgba(60,40,0,.08),0 6px 18px rgba(60,40,0,.06);
  color-scheme:light dark;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0a0b0d;--card:#15181d;--line:#1f242b;--line-strong:#2a313a;
    --fg:#e6e7e9;--muted:#a4a8ae;--band:#251d0a;--band-fg:#f4efe2;
    --code-bg:#1f242b;--accent:#e25a3c;--accent-text:#ec6345;--focus:#ffc244;
    --shadow:none;
  }
}
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;min-height:100vh;background:var(--bg);color:var(--fg);font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;font-size:1rem;line-height:1.55}
.band{background:var(--band);color:var(--band-fg);border-bottom:1px solid var(--line-strong)}
.wrap{max-width:46rem;margin:0 auto;padding-inline:16px}
.band .wrap{padding-block:clamp(28px,6vw,56px) clamp(20px,4vw,32px)}
.band h1{margin:0;font-size:clamp(1.75rem,5.5vw,2.75rem);line-height:1.1;letter-spacing:-.01em;text-wrap:balance}
.band h1::before{content:"";display:block;width:36px;height:4px;margin-bottom:14px;border-radius:2px;background:var(--accent)}
.band p{margin:14px 0 0}
main{padding-block:clamp(20px,4vw,40px) 48px}
section{margin:0 0 20px;padding:18px 20px;border:1px solid var(--line);border-radius:14px;background:var(--card);box-shadow:var(--shadow);overflow-wrap:anywhere}
section>:first-child{margin-top:0}
section>:last-child{margin-bottom:0}
h2{margin:0 0 12px;font-size:clamp(1.25rem,3.5vw,1.5rem);line-height:1.2}
p{margin:0 0 12px}
ul,ol{margin:0 0 12px;padding-left:1.4em}
li{margin:0 0 6px}
li::marker{color:var(--accent-text)}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.9em;color:var(--fg);background:var(--code-bg);border-radius:6px;padding:2px 6px;overflow-wrap:anywhere}
.code{display:block;margin:0 0 12px;padding:14px 16px;border:2px solid var(--accent);border-radius:12px;background:var(--card);font-size:clamp(1.05rem,4.5vw,1.5rem);font-weight:700;letter-spacing:.04em;color:var(--accent-text);word-break:break-all}
a{color:var(--accent-text);text-underline-offset:3px}
a:focus-visible,button:focus-visible,input:focus-visible,select:focus-visible,textarea:focus-visible{outline:3px solid var(--focus);outline-offset:3px;border-radius:4px}
@media (max-width:480px){section{padding:16px;border-radius:12px}}
@media print{.band{background:none}}
`

// pageCSP is the Content-Security-Policy of the HTML pages: everything is
// denied except the one inline <style>, named by the hash of pageCSS. No
// 'unsafe-inline', no nonce, no script-src (default-src 'none' covers it).
var pageCSP = cspForStyle(pageCSS)

func cspForStyle(css string) string {
	sum := sha256.Sum256([]byte(css))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}
