package plugins

import (
	"strings"
	"testing"
)

func TestPHPSlowlogConfigurationIsTypedBoundedAndDisabledByDefault(t *testing.T) {
	config := defaultConfig("php-apt")
	if config["slowlog_enabled"] != false || config["slowlog_timeout_seconds"] != 5 {
		t.Fatalf("PHP slowlog defaults are not explicit and disabled: %#v", config)
	}
	if err := validateConfiguration("php-apt", config); err != nil {
		t.Fatal(err)
	}
	if _, err := renderPHPIni(config, "/managed/php/sessions"); err != nil {
		t.Fatal(err)
	}
	instance := Instance{Paths: Paths{LogDir: "/managed/php/logs"}, Config: config}
	body := renderDefaultPHPPool(instance, "php-worker", "php-worker", true)
	if strings.Contains(body, "slowlog") {
		t.Fatalf("disabled slowlog appeared in the FPM pool:\n%s", body)
	}
	config["slowlog_enabled"] = true
	config["slowlog_timeout_seconds"] = 17
	instance.Config = config
	body = renderDefaultPHPPool(instance, "php-worker", "php-worker", true)
	if !strings.Contains(body, "request_slowlog_timeout = 17s\n") || !strings.Contains(body, "slowlog = /managed/php/logs/fpm-slow.log\n") {
		t.Fatalf("enabled slowlog was not rendered to the trusted log directory:\n%s", body)
	}
	for key, value := range map[string]any{"slowlog_enabled": "yes", "slowlog_timeout_seconds": 301} {
		bad := cloneConfig(config)
		bad[key] = value
		if err := validateConfiguration("php-apt", bad); err == nil {
			t.Fatalf("invalid slowlog value %s=%v was accepted", key, value)
		}
	}
}
