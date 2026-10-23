package serverbrowser

import (
	"embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aoody743/PowerLinux/internal/core"
)

//go:embed notices/*
var browserNotices embed.FS

// Only the pinned release is patched. Assert each expected source fragment so
// an upstream layout change fails installation instead of using a public proxy.
func prepareWasm(root string) error {
	path := filepath.Join(root, "assets", "index-D39giZCc.js")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	source := string(raw)
	patches := [][2]string{
		{`let y=de.href;const F=!0;F&&await fetch("https://sensible-ship-8305.puter.work/").then(r=>r.text()).then(r=>{y=r.trim()});`, `let y=de.href;const F=!0;`},
		{`new WebSocket(url)`, `new WebSocket(url, [\"folio-wisp-v1\", globalThis.folioWispProtocol])`},
		{`const Qe="chrome-demo-opts"`, `const Qe="folio-firefox-wasm-options-v1"`},
		{`aA="profile"`, `aA=globalThis.folioBrowserProfile`},
		{`const t="chrome-demo-search-seeded-v2"`, `const t="folio-firefox-search-"+globalThis.folioBrowserProfile`},
		{`A.evalChrome("openTrustedLinkIn('https://developer.puter.com/', 'current'); 'ok'")`, `A.evalChrome("openTrustedLinkIn('about:blank', 'current'); 'ok'")`},
	}
	for _, patch := range patches {
		if strings.Count(source, patch[0]) != 1 {
			return fmt.Errorf("unsupported Firefox WASM bootstrap: expected one %q", patch[0])
		}
		source = strings.Replace(source, patch[0], patch[1], 1)
	}
	if strings.Count(source, "window.innerHeight") != 3 {
		return fmt.Errorf("unsupported Firefox viewport bootstrap")
	}
	source = strings.ReplaceAll(source, "window.innerHeight", "Math.max(240,window.innerHeight-40)")
	if err = os.WriteFile(path, []byte(source), 0600); err != nil {
		return err
	}
	html, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		return err
	}
	text := string(html)
	start := strings.Index(text, `    <link rel="preconnect"`)
	end := strings.Index(text, `    <script type="module"`)
	if start < 0 || end <= start {
		return fmt.Errorf("unsupported Firefox WASM HTML")
	}
	text = text[:start] + text[end:]
	text = strings.Replace(text, `src="./assets/index-D39giZCc.js"`, `src="./launch.js"`, 1)
	text = strings.ReplaceAll(text, "Web content is proxied through a Puter-hosted Wisp server.", "Web content uses this Folio server's network. / 网页通过本面板服务器的网络访问。")
	text = strings.ReplaceAll(text, "Launch Firefox", "打开 Firefox / Open Firefox")
	text = strings.ReplaceAll(text, "Advanced options", "高级选项 / Advanced options")
	text = strings.ReplaceAll(text, "<title>Firefox in WebAssembly</title>", "<title>Folio · Firefox WASM</title>")
	text = strings.ReplaceAll(text, "Networking in the browser powered by <strong>Puter.js</strong>", "Firefox WASM by <strong>Puter</strong> · Folio server network")
	text = strings.Replace(text, `<section id="splash"`, `<nav id="folio-bar"><a id="folio-back" href="/#/software?page=applications&amp;subgroup=browsers">返回面板 / Back to Folio</a><span>Firefox WASM</span><span id="folio-network">服务器网络 / Server network</span><button id="folio-fullscreen" type="button">全屏 / Full screen</button><a href="NOTICE.txt" target="_blank" rel="noopener">许可 / Licenses</a></nav><section id="splash"`, 1)
	if err = os.WriteFile(filepath.Join(root, "index.html"), []byte(text), 0600); err != nil {
		return err
	}
	for _, name := range []string{"NOTICE.txt", "MPL-2.0.txt"} {
		body, e := browserNotices.ReadFile("notices/" + name)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(root, name), body, 0600); e != nil {
			return e
		}
	}
	return os.WriteFile(filepath.Join(root, "launch.js"), []byte(wasmLaunch), 0600)
}

const wasmLaunch = `
const zh=(localStorage.getItem('folio.language')||navigator.language).startsWith('zh');
const status=document.getElementById('splash-status');
const style=document.createElement('style');style.textContent='#folio-bar{position:fixed;inset:0 0 auto;height:40px;z-index:10000;display:flex;align-items:center;gap:12px;padding:0 12px;background:#202735;color:#fff;font:14px system-ui}#folio-bar a{color:#fff}#folio-bar button{font:inherit;margin-left:auto}#folio-network{color:#c6d0df}#screen{height:calc(100dvh - 40px)!important}body>#screen{position:fixed;top:40px;left:0}[data-libxul-glwrap="1"]{position:fixed!important;top:40px;left:0}.top-nav{top:40px}#splash{padding-top:40px}#folio-bar a:last-child{font-size:12px}@media(max-width:600px){#folio-network,#folio-bar a:last-child{display:none}#folio-bar{gap:8px}}';document.head.append(style);
document.getElementById('folio-back').textContent=zh?'返回面板':'Back to Folio';
document.getElementById('folio-network').textContent=zh?'服务器网络':'Server network';
const full=document.getElementById('folio-fullscreen');full.textContent=zh?'全屏':'Full screen';full.onclick=async()=>{try{if(document.fullscreenElement)await document.exitFullscreen();else await document.documentElement.requestFullscreen()}catch{status.textContent=zh?'当前浏览器不支持全屏。':'Full screen is unavailable in this browser.'}};
document.addEventListener('fullscreenchange',()=>full.textContent=document.fullscreenElement?(zh?'退出全屏':'Exit full screen'):(zh?'全屏':'Full screen'));
if(zh){document.documentElement.lang='zh-CN';document.getElementById('project-title').textContent='Firefox 浏览器';document.querySelector('.project-copy').textContent='完整 Firefox 界面，网页通过本面板服务器的网络访问。首次加载可能需要一点时间。';document.querySelector('.advanced summary').textContent='高级选项';document.querySelector('.branding-note').textContent='使用本面板服务器的网络出口。Firefox 配置保存在当前浏览器，卸载服务器组件不会清除本机配置。';}

try {
 if(!isSecureContext) throw new Error(zh?'请使用 HTTPS 打开面板后运行 Firefox WASM。':'Open the panel over HTTPS to run Firefox WASM.');
 if(typeof WebAssembly.Suspending!=='function'||typeof WebAssembly.promising!=='function') throw new Error(zh?'当前浏览器不支持 JSPI，请使用新版 Chrome/Edge，或返回面板选择面板模式。':'JSPI is unavailable. Use a recent Chrome/Edge, or return to Folio and choose panel mode.');
 const response=await fetch('/api/v1/session',{credentials:'same-origin',cache:'no-store'});
 const session=(await response.json()).data;
 if(!session?.authenticated||!session.csrf_token) throw new Error(zh?'请先登录面板，再重新打开 Firefox。':'Sign in to Folio, then reopen Firefox.');
 globalThis.folioWispProtocol='powerlinux-csrf.'+session.csrf_token;
 const ownerHash=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(session.user.id));
 globalThis.folioBrowserProfile='folio-firefox-'+Array.from(new Uint8Array(ownerHash),n=>n.toString(16).padStart(2,'0')).join('');
 // Same-origin Wisp is the sole network transport. No public Wisp fallback.
 const NativeWebSocket=globalThis.WebSocket;
 globalThis.WebSocket=class extends NativeWebSocket {
  constructor(url,protocols){const u=new URL(url,location.href);const expected=new URL('wisp/',location.href);expected.protocol=location.protocol==='https:'?'wss:':'ws:';if(u.href!==expected.href)throw new Error('Only Folio Wisp is permitted');super(u.href,protocols);}
 };
 await import('./assets/index-D39giZCc.js');
 document.getElementById('start-btn').textContent=zh?'打开 Firefox':'Open Firefox';
 if(zh){document.querySelectorAll('.option-copy strong')[0].textContent='GPU 加速';document.querySelectorAll('.option-copy strong')[1].textContent='启用 JIT（实验性）';}

} catch(error) {
 status.textContent=error.message;
 document.getElementById('start-btn').disabled=true;
 document.getElementById('progress-phase').textContent=zh?'无法启动':'Cannot start';
}
`

func (p *Provider) handleWasm(w http.ResponseWriter, r *http.Request, a core.Actor) bool {
	const prefix = "/api/v1/browser/firefox-wasm/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "wisp/" {
		p.serveWisp(w, r, a)
		return true
	}
	p.appGate.RLock()
	defer p.appGate.RUnlock()
	if !p.installed(r.Context(), "firefox-wasm") {
		reply(w, failure("browser_not_installed", "Install Firefox WASM from Software → Applications first", 409))
		return true
	}
	// Explicit file allowlist; never expose arbitrary app data or profiles.
	if name == "" {
		name = "index.html"
	}
	allowed := map[string]string{"index.html": "text/html; charset=utf-8", "launch.js": "text/javascript; charset=utf-8", "assets/index-D39giZCc.js": "text/javascript; charset=utf-8", "assets/index-CJZf4zMx.css": "text/css; charset=utf-8", "chrome-assets.json": "application/json", "chrome-assets.tar.zst": "application/octet-stream", "gecko.wasm.zst": "application/octet-stream", "logo.webp": "image/webp", "NOTICE.txt": "text/plain; charset=utf-8", "MPL-2.0.txt": "text/plain; charset=utf-8"}
	contentType, ok := allowed[name]
	if !ok {
		reply(w, failure("not_found", "Firefox asset not found", 404))
		return true
	}
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' blob: 'unsafe-eval' 'wasm-unsafe-eval'; worker-src 'self' blob:; connect-src 'self' blob: data:; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Content-Type", contentType)
	f, err := os.Open(filepath.Join(p.options.AppsDir, "firefox-wasm", filepath.FromSlash(name)))
	if err != nil {
		reply(w, failure("browser_unavailable", "Firefox assets are missing; uninstall and install this app again", 503))
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		reply(w, failure("not_found", "Firefox asset not found", 404))
		return true
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
	return true
}
