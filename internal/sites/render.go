package sites

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Identity struct {
	User  string
	Group string
	UID   uint32
	GID   uint32
}
type renderedPaths struct{ DocumentRoot, LogDir, RunDir, TempDir, Certificate, PrivateKey, PasswordFile, ZoneTag, ParentID string }

func proxyCABundle() string {
	for _, path := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return "/etc/ssl/certs/ca-certificates.crt"
}

// Nginx strings contain trusted paths or previously validated structured
// values. Dollar signs are escaped here; only constant template variables are
// emitted unescaped by this package.
func nginxString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`).Replace(value) + `"`
}
func iniString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(value) + `"`
}
func siteTag(id string) string { sum := sha256.Sum256([]byte(id)); return hex.EncodeToString(sum[:8]) }

func renderWebsite(site Site, paths renderedPaths, web WebRuntime, fpmAddress string, now time.Time) (string, error) {
	if err := validateSite(&site); err != nil {
		return "", err
	}
	if site.ExpiresAt != "" && !web.SupportsLua {
		return "", invalid("Automatic website expiration requires the OpenResty stack; clear expires_at or select an OpenResty runtime")
	}
	if !filepath.IsAbs(paths.DocumentRoot) || !filepath.IsAbs(paths.LogDir) {
		return "", invalid("Website Agent paths must be absolute")
	}
	var out strings.Builder
	tag := siteTag(site.ID)
	if paths.ZoneTag != "" {
		tag = paths.ZoneTag
	}
	for _, proxy := range site.Proxies {
		if paths.ZoneTag == "" && proxy.Enabled && proxy.CacheSeconds > 0 {
			cacheTag := siteTag(tag + ":" + proxy.ID)
			fmt.Fprintf(&out, "proxy_cache_path %s levels=1:2 keys_zone=pl_cache_%s:1m max_size=64m inactive=60m use_temp_path=off;\n", nginxString(filepath.Join(paths.RunDir, "proxy-cache", cacheTag)), cacheTag)
		}
	}
	if paths.ZoneTag == "" && site.RateLimit.Enabled && site.RateLimit.Connections > 0 {
		fmt.Fprintf(&out, "limit_conn_zone $binary_remote_addr zone=pl_conn_%s:1m;\n", tag)
	}
	if paths.ZoneTag == "" && site.RateLimit.Enabled && site.RateLimit.RequestsPerSecond > 0 {
		fmt.Fprintf(&out, "limit_req_zone $binary_remote_addr zone=pl_req_%s:1m rate=%dr/s;\n", tag, site.RateLimit.RequestsPerSecond)
	}
	groups := map[int][]string{}
	for _, value := range site.Domains {
		_, host, port, err := domain(value)
		if err != nil {
			return "", err
		}
		groups[port] = append(groups[port], host)
	}
	ports := make([]int, 0, len(groups))
	for port := range groups {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	tlsNames := []string{}
	for _, port := range ports {
		hosts := groups[port]
		if site.TLS.Enabled {
			if paths.Certificate == "" || paths.PrivateKey == "" {
				return "", invalid("TLS certificate is not deployed")
			}
			if port != 443 {
				writeServerHeader(&out, port, site.Default, site.IPv6, false, hosts)
				if site.TLS.Redirect {
					renderHealthHeaders(&out, site, paths)
					out.WriteString("    return 308 https://$host$request_uri;\n")
				} else {
					renderServerBody(&out, site, paths, web, fpmAddress, tag, now)
				}
				out.WriteString("}\n")
			}
			tlsNames = append(tlsNames, hosts...)
		} else {
			writeServerHeader(&out, port, site.Default, site.IPv6, false, hosts)
			renderServerBody(&out, site, paths, web, fpmAddress, tag, now)
			out.WriteString("}\n")
		}
	}
	if site.TLS.Enabled {
		sort.Strings(tlsNames)
		tlsNames = slices.Compact(tlsNames)
		writeServerHeader(&out, 443, site.Default, site.IPv6, true, tlsNames)
		fmt.Fprintf(&out, "    ssl_certificate %s;\n    ssl_certificate_key %s;\n    ssl_protocols TLSv1.2 TLSv1.3;\n", nginxString(paths.Certificate), nginxString(paths.PrivateKey))
		out.WriteString("    ssl_session_cache shared:FolioTLS:1m;\n    ssl_session_timeout 10m;\n")
		renderServerBody(&out, site, paths, web, fpmAddress, tag, now)
		out.WriteString("}\n")
	}
	for _, binding := range site.DirectoryBindings {
		bound := site
		bound.ID = "s-" + siteTag(site.ID+"-"+binding.ID)
		bound.Domains = binding.Domains
		bound.DirectoryBindings = nil
		bound.Default = false
		bound.RunPath = "."
		bound.Rewrite = binding.Rewrite
		boundPaths := paths
		boundPaths.ZoneTag = tag
		if boundPaths.ParentID == "" {
			boundPaths.ParentID = site.ID
		}
		boundPaths.DocumentRoot = filepath.Join(paths.DocumentRoot, binding.Path)
		config, err := renderWebsite(bound, boundPaths, web, fpmAddress, now)
		if err != nil {
			return "", err
		}
		out.WriteString(config)
	}
	return out.String(), nil
}

func writeServerHeader(out *strings.Builder, port int, defaultServer, ipv6, ssl bool, domains []string) {
	out.WriteString("server {\n")
	suffix := ""
	if ssl {
		suffix += " ssl"
	}
	if defaultServer {
		suffix += " default_server"
	}
	fmt.Fprintf(out, "    listen %d%s;\n", port, suffix)
	if ipv6 {
		fmt.Fprintf(out, "    listen [::]:%d%s;\n", port, suffix)
	}
	// Normalized IPs need Nginx's bracket notation in server_name.
	values := []string{}
	for _, host := range domains {
		if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		values = append(values, host)
	}
	fmt.Fprintf(out, "    server_name %s;\n", strings.Join(values, " "))
}

func renderServerBody(out *strings.Builder, site Site, paths renderedPaths, web WebRuntime, fpmAddress, tag string, now time.Time) {
	renderHealthHeaders(out, site, paths)
	if site.Status == "stopped" {
		out.WriteString("    return 503;\n")
		return
	}
	if site.ExpiresAt != "" {
		expiry, _ := time.Parse(time.RFC3339, site.ExpiresAt)
		fmt.Fprintf(out, "    server_rewrite_by_lua_block { if ngx.time() >= %d and not ngx.var.uri:find('^/%%.well%%-known/acme%%-challenge/') then return ngx.exit(410) end }\n", expiry.Unix())
	}
	fmt.Fprintf(out, "    root %s;\n    index %s;\n", nginxString(filepath.Join(paths.DocumentRoot, site.RunPath)), strings.Join(site.Index, " "))
	if site.AccessLog {
		fmt.Fprintf(out, "    access_log %s;\n", nginxString(filepath.Join(paths.LogDir, "access.log")))
	} else {
		out.WriteString("    access_log off;\n")
	}
	fmt.Fprintf(out, "    error_log %s warn;\n    client_max_body_size 64m;\n", nginxString(filepath.Join(paths.LogDir, "error.log")))
	out.WriteString("    location ^~ /.well-known/acme-challenge/ { auth_basic off; allow all; try_files $uri =404; }\n    location ~ /\\.(?!well-known) { deny all; }\n")
	if site.RateLimit.Enabled {
		if site.RateLimit.Connections > 0 {
			fmt.Fprintf(out, "    limit_conn pl_conn_%s %d;\n", tag, site.RateLimit.Connections)
		}
		if site.RateLimit.RequestsPerSecond > 0 {
			fmt.Fprintf(out, "    limit_req zone=pl_req_%s burst=%d nodelay;\n", tag, max(1, site.RateLimit.RequestsPerSecond))
		}
		if site.RateLimit.BytesPerSecond > 0 {
			fmt.Fprintf(out, "    limit_rate %d;\n", site.RateLimit.BytesPerSecond)
		}
	}
	if site.Protection.Enabled {
		fmt.Fprintf(out, "    auth_basic \"Restricted\";\n    auth_basic_user_file %s;\n", nginxString(paths.PasswordFile))
	}
	for _, value := range site.Protection.DenyPaths {
		fmt.Fprintf(out, "    location ^~ %s { deny all; }\n", value)
	}
	if site.Protection.AntiHotlink {
		out.WriteString("    valid_referers none blocked server_names")
		for _, host := range site.Protection.AllowedReferers {
			_, normal, _, _ := domain(host)
			out.WriteString(" " + normal)
		}
		out.WriteString(";\n    if ($invalid_referer) { return 403; }\n")
	}
	rootRule := false
	for _, redirect := range site.Redirects {
		if !redirect.Enabled {
			continue
		}
		if redirect.Path == "/" {
			rootRule = true
		}
		suffix := ""
		if redirect.PreserveURI {
			suffix = "$request_uri"
		}
		destination := nginxString(redirect.Destination)
		if suffix != "" {
			destination = strings.TrimSuffix(destination, `"`) + suffix + `"`
		}
		fmt.Fprintf(out, "    location %s { return %d %s; }\n", redirect.Path, redirect.Status, destination)
	}
	for _, proxy := range site.Proxies {
		if !proxy.Enabled {
			continue
		}
		if proxy.Path == "/" {
			rootRule = true
		}
		fmt.Fprintf(out, "    location %s {\n        proxy_pass %s;\n        proxy_http_version 1.1;\n        proxy_connect_timeout 10s;\n        proxy_read_timeout 60s;\n", proxy.Path, nginxString(proxy.Upstream))
		if upstream, err := url.Parse(proxy.Upstream); err == nil && upstream.Scheme == "https" {
			fmt.Fprintf(out, "        proxy_ssl_server_name on;\n        proxy_ssl_verify on;\n        proxy_ssl_verify_depth 5;\n        proxy_ssl_name %s;\n        proxy_ssl_trusted_certificate %s;\n", nginxString(upstream.Hostname()), nginxString(proxyCABundle()))
		}
		if proxy.PreserveHost {
			out.WriteString("        proxy_set_header Host $host;\n")
		}
		out.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n        proxy_set_header X-Forwarded-Proto $scheme;\n")
		if proxy.WebSocket {
			out.WriteString("        proxy_set_header Upgrade $http_upgrade;\n        proxy_set_header Connection \"upgrade\";\n")
		}
		keys := []string{}
		for key := range proxy.Headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(out, "        proxy_set_header %s %s;\n", key, nginxString(proxy.Headers[key]))
		}
		if proxy.CacheSeconds > 0 {
			fmt.Fprintf(out, "        proxy_cache pl_cache_%s;\n        proxy_cache_key \"$scheme|$proxy_host|$host|$request_uri\";\n        proxy_cache_valid 200 301 302 %ds;\n        proxy_cache_bypass $http_authorization $http_cookie;\n        proxy_no_cache $http_authorization $http_cookie;\n        proxy_cache_lock on;\n        proxy_cache_use_stale error timeout updating http_500 http_502 http_503 http_504;\n", siteTag(tag+":"+proxy.ID), proxy.CacheSeconds)
		}
		out.WriteString("    }\n")
	}
	if !rootRule {
		out.WriteString("    location / {\n")
		if site.Rewrite == "" {
			out.WriteString("        try_files $uri $uri/ =404;\n")
		} else {
			for _, line := range strings.Split(site.Rewrite, "\n") {
				out.WriteString("        " + line + "\n")
			}
		}
		out.WriteString("    }\n")
	}
	if site.PHPInstance != "" {
		fmt.Fprintf(out, "    location ~ \\.php$ {\n        try_files $uri =404;\n        include %s;\n        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;\n        fastcgi_pass %s;\n    }\n", nginxString(filepath.Join(filepath.Dir(web.MainConfig), "fastcgi_params")), nginxString(fpmAddress))
	} else {
		out.WriteString("    location ~ \\.php$ { return 404; }\n")
	}
}

func renderHealthHeaders(out *strings.Builder, site Site, paths renderedPaths) {
	id := site.ID
	if paths.ParentID != "" {
		id = paths.ParentID
	}
	fmt.Fprintf(out, "    add_header X-Folio-Site %s always;\n    add_header X-Folio-Revision %d always;\n", nginxString(id), site.Revision)
	out.WriteString("    location = /.__powerlinux_health { auth_basic off; access_log off; return 204; }\n")
}
func renderPool(site Site, paths renderedPaths, worker Identity, web WebRuntime) (string, error) {
	if err := validateSite(&site); err != nil {
		return "", err
	}
	if worker.UID == 0 || worker.User == "" || worker.Group == "" || web.WorkerUser == "" || web.WorkerGroup == "" {
		return "", invalid("PHP pool needs an isolated non-root website identity and trusted Web identity")
	}
	var out strings.Builder
	fmt.Fprintf(&out, "[powerlinux-%s]\nuser = %s\ngroup = %s\nlisten = %s\nlisten.owner = %s\nlisten.group = %s\nlisten.mode = 0660\npm = ondemand\npm.max_children = 4\npm.process_idle_timeout = 20s\npm.max_requests = 500\nclear_env = yes\nsecurity.limit_extensions = .php\n", siteTag(site.ID), worker.User, worker.Group, iniString(filepath.Join(paths.RunDir, "php-fpm.sock")), web.WorkerUser, web.WorkerGroup)
	fmt.Fprintf(&out, "php_admin_value[error_log] = %s\nphp_admin_flag[log_errors] = on\nphp_admin_value[upload_tmp_dir] = %s\nphp_admin_value[session.save_path] = %s\n", iniString(filepath.Join(paths.TempDir, "php-errors.log")), iniString(paths.TempDir), iniString(paths.TempDir))
	if site.PHP.SlowlogEnabled {
		fmt.Fprintf(&out, "request_slowlog_timeout = %ds\nslowlog = %s\n", site.PHP.SlowlogTimeoutSeconds, filepath.Join(paths.LogDir, "php-slow.log"))
	}
	if site.PHP.OpenBasedir {
		fmt.Fprintf(&out, "php_admin_value[open_basedir] = %s\n", iniString(paths.DocumentRoot+string(filepath.ListSeparator)+paths.TempDir))
	}
	keys := []string{}
	for key := range site.PHP.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "open_basedir" || key == "error_log" || key == "upload_tmp_dir" || key == "session.save_path" {
			return "", invalid("Use the typed PHP directory settings instead of replacing protected pool paths")
		}
		fmt.Fprintf(&out, "php_admin_value[%s] = %s\n", key, iniString(site.PHP.Values[key]))
	}
	return out.String(), nil
}

func parseHTTPPort(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > 65535 {
		return 0, invalid("Invalid HTTP port")
	}
	return number, nil
}
