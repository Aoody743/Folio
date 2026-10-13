package terminalbridge

import "github.com/Aoody743/PowerLinux/internal/core"

func object(fields map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": fields, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
func text(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
func number(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max}
}
func descriptors() []core.Action {
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": `^[a-zA-Z0-9_-]+$`}
	secret := map[string]any{"type": "string", "maxLength": 32768, "writeOnly": true, "format": "password"}
	fields := map[string]any{"id": id, "target_id": id, "label": text(128), "host": text(253), "port": number(1, 65535), "username": text(64), "auth_type": map[string]any{"type": "string", "enum": []string{"password", "key"}}, "host_key": text(128), "password": secret, "private_key": secret, "passphrase": secret, "expected_revision": number(0, 1<<30), "title": text(128), "command": text(4096), "rows": number(1, 500), "cols": number(1, 1000), "after": number(0, 1<<53-1), "limit": number(1, 8), "data": text(21848)}
	rows := []struct {
		name, scope, description string
		read                     bool
		fields, required         []string
	}{
		{"terminal_targets_list", "terminal:connect", "读取授权的终端连接目标", true, nil, nil},
		{"terminal_targets_get", "terminal:connect", "读取一个终端连接目标", true, []string{"id"}, []string{"id"}},
		{"terminal_targets_create", "terminal:manage", "创建校验主机密钥并绑定新凭据的SSH目标", false, []string{"label", "host", "port", "username", "auth_type", "host_key", "password", "private_key", "passphrase"}, []string{"label", "host", "username", "auth_type", "host_key"}},
		{"terminal_targets_update", "terminal:manage", "按基线版本更新SSH目标；身份变化需要新凭据", false, []string{"id", "expected_revision", "label", "host", "port", "username", "auth_type", "host_key", "password", "private_key", "passphrase"}, []string{"id", "expected_revision"}},
		{"terminal_targets_delete", "terminal:manage", "删除无活动会话的SSH目标", false, []string{"id"}, []string{"id"}},
		{"terminal_commands_list", "terminal:connect", "读取授权的快捷命令", true, nil, nil},
		{"terminal_commands_create", "terminal:manage", "保存快捷命令，不直接执行", false, []string{"id", "expected_revision", "title", "command"}, []string{"title", "command"}},
		{"terminal_commands_delete", "terminal:manage", "删除快捷命令", false, []string{"id"}, []string{"id"}},
		{"terminal_open", "terminal:connect", "连接受管理终端", false, []string{"target_id", "rows", "cols"}, []string{"target_id"}},
		{"terminal_root_open", "terminal:root", "连接明确授权的Root终端", false, []string{"target_id", "rows", "cols"}, []string{"target_id"}},
		{"terminal_share_discover", "terminal:share", "读取已获共享权限的会话信息", true, []string{"id"}, []string{"id"}},
		{"terminal_read", "terminal:connect", "读取有界终端输出帧", true, []string{"id", "after", "limit"}, []string{"id"}},
		{"terminal_input", "terminal:connect", "发送一帧有界终端输入", false, []string{"id", "data"}, []string{"id", "data"}},
		{"terminal_resize", "terminal:connect", "调整终端显示尺寸", false, []string{"id", "rows", "cols"}, []string{"id", "rows", "cols"}},
		{"terminal_renew", "terminal:connect", "重新验证并延长终端租约", false, []string{"id"}, []string{"id"}},
		{"terminal_close", "terminal:connect", "关闭终端会话", false, []string{"id"}, []string{"id"}},
	}
	result := []core.Action{}
	for _, row := range rows {
		properties := map[string]any{}
		for _, name := range row.fields {
			properties[name] = fields[name]
		}
		result = append(result, core.Action{Name: row.name, Scope: row.scope, Description: row.description, ReadOnly: row.read, InputSchema: object(properties, row.required...)})
	}
	return result
}
