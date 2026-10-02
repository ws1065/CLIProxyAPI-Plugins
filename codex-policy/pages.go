package main

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
)

const pageStyle = `<style>body{font:13px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;margin:0;padding:24px;background:#f6f7f9;color:#1c1e21}h1{font-size:18px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px}.sub{color:#6b7076;margin-bottom:18px}table{border-collapse:collapse;width:100%;background:#fff;box-shadow:0 1px 2px rgba(0,0,0,.08);border-radius:6px;overflow:hidden}th,td{padding:7px 10px;border-bottom:1px solid #eceef1;text-align:left;vertical-align:top;font-size:12px}th{background:#fafbfc;font-weight:600;color:#5a5f66;white-space:nowrap}tr:last-child td{border-bottom:none}code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}.k{width:230px;font-weight:600}.tag{display:inline-block;padding:1px 7px;border-radius:10px;font-size:11px;background:#e8f0fe;color:#1a56c4;margin-right:4px}.tag.warn{background:#fdecea;color:#b3261e}.tag.ok{background:#e6f4ea;color:#137333}.tag.mute{background:#eceef1;color:#5a5f66}.cards{display:flex;gap:10px;flex-wrap:wrap;margin-bottom:14px}.card{background:#fff;border:1px solid #e6e8eb;border-radius:6px;padding:10px 14px;min-width:92px}.card .n{font-size:19px;font-weight:600}.card .l{color:#6b7076;font-size:11px}.empty{background:#fff;padding:28px;text-align:center;color:#6b7076;border-radius:6px}.note{background:#fff8e6;border-left:3px solid #f0b429;padding:8px 12px;margin:12px 0;font-size:12px}</style>`

// configScript powers the save button. It submits only the fields the user
// actually changed, so a value the page cannot round-trip is never written
// back. The management key is reused from the panel's localStorage when the
// page is opened inside the same origin; otherwise it can be pasted once.
const configScript = `<script>(function () {
  var form = document.getElementById('cfg');
  if (!form) { return; }
  var dirty = {};
  var inputs = form.querySelectorAll('[data-key]');
  function markDirty(el) {
    var type = el.getAttribute('data-type');
    dirty[el.getAttribute('data-key')] = (type === 'boolean') ? el.checked : el.value;
    el.style.background = '#fff8e6';
  }
  for (var i = 0; i < inputs.length; i++) {
    var el = inputs[i];
    var ev = (el.type === 'checkbox' || el.tagName === 'SELECT') ? 'change' : 'input';
    el.addEventListener(ev, (function (node) {
      return function () { markDirty(node); };
    })(el));
  }
  function managementKey() {
    var manual = document.getElementById('mgmtkey');
    if (manual && manual.value.trim()) { return manual.value.trim(); }
    var names = ['managementKey', 'managementKeys', 'management_key', 'cpa-management-key'];
    for (var i = 0; i < names.length; i++) {
      var raw = null;
      try { raw = window.localStorage.getItem(names[i]); } catch (e) { raw = null; }
      if (!raw) { continue; }
      try {
        var parsed = JSON.parse(raw);
        if (Array.isArray(parsed) && parsed.length) { return String(parsed[0]); }
        if (typeof parsed === 'string') { return parsed; }
      } catch (e) { return raw; }
      return raw;
    }
    return '';
  }
  document.getElementById('save').addEventListener('click', function () {
    var keys = Object.keys(dirty);
    var hint = document.getElementById('hint');
    if (!keys.length) { hint.innerHTML = '<span class="tag mute">没有改动</span>'; return; }
    var headers = { 'Content-Type': 'application/json' };
    var key = managementKey();
    if (key) { headers['Authorization'] = 'Bearer ' + key; }
    fetch('/v0/management/plugins/codex-policy/settings', {
      method: 'POST', headers: headers, body: JSON.stringify(dirty)
    }).then(function (resp) {
      return resp.json().then(function (body) { return { ok: resp.ok, status: resp.status, body: body }; });
    }).then(function (res) {
      if (res.ok && res.body && res.body.current) {
        hint.innerHTML = '<span class="tag ok">已保存 ' + keys.length + ' 项，正在刷新</span>';
        setTimeout(function () { window.location.reload(); }, 700);
        return;
      }
      if (res.status === 401 || res.status === 403) {
        hint.innerHTML = '<span class="tag warn">需要管理密钥：请在下方输入框粘贴宿主 management.secret-key</span>';
        var box = document.getElementById('mgmtkey');
        if (box) { box.style.borderColor = '#b3261e'; box.focus(); }
        return;
      }
      var msg = (res.body && (res.body.message || res.body.error)) || ('HTTP ' + res.status);
      hint.innerHTML = '<span class="tag warn">保存失败：' + String(msg) + '</span>';
    }).catch(function (err) {
      hint.innerHTML = '<span class="tag warn">保存失败：' + String(err) + '</span>';
    });
  });
})();</script>`

func pageHeader(title, subtitle string) string {
	return "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + html.EscapeString(title) + "</title>" + pageStyle + "</head><body>" +
		"<h1>" + html.EscapeString(title) + "</h1><div class=\"sub\">" + html.EscapeString(subtitle) + "</div>"
}

func renderLogsPage(entries []logEntry, stats logStats, limit int) string {
	var b strings.Builder
	b.WriteString(pageHeader("codex-policy · 请求日志", fmt.Sprintf("插件 v%s · 显示最近 %d 条（上限 %d，内存缓存，重启清空）· API key 已脱敏", pluginVersion, len(entries), limit)))
	b.WriteString(`<div class="cards">`)
	for _, card := range []struct {
		n    int
		name string
	}{{stats.Admitted, "放行"}, {stats.Rejected, "拒绝"}, {stats.Mapped, "模型映射"}, {stats.Downgrade, "思考降级"}, {stats.Exempt, "豁免 key"}, {stats.Total, "累计请求"}} {
		fmt.Fprintf(&b, `<div class="card"><div class="n">%d</div><div class="l">%s</div></div>`, card.n, card.name)
	}
	b.WriteString(`</div>`)
	if len(entries) == 0 {
		b.WriteString(`<div class="empty">还没有请求记录。发一次请求后再回来查看。<br><span class="mono">curl .../v1/responses</span></div>`)
		return b.String() + "</body></html>"
	}
	b.WriteString(`<table><thead><tr><th>时间</th><th>API key</th><th>客户端</th><th>请求模型</th><th>实际转发</th><th>思考强度</th><th>准入</th><th>处理动作</th><th>结果</th></tr></thead><tbody>`)
	for _, entry := range entries {
		routed := `<span class="tag mute">未映射</span>`
		if entry.RoutedModel != "" {
			routed = `<span class="mono">` + html.EscapeString(entry.RoutedModel) + `</span> <span class="tag mute">` + html.EscapeString(entry.Provider) + `</span>`
		}
		effort := `<span class="tag mute">` + html.EscapeString(orDash(entry.Effort)) + `</span>`
		if entry.EffortFinal != "" && entry.EffortFinal != entry.Effort {
			effort = `<span class="mono">` + html.EscapeString(entry.Effort) + `</span> <span class="tag warn">→ ` + html.EscapeString(entry.EffortFinal) + `</span>`
		}
		admission := `<span class="tag ok">ALLOWED ` + strconv.Itoa(entry.Score) + `/` + strconv.Itoa(entry.MinScore) + `</span>`
		if !entry.Admitted {
			admission = `<span class="tag warn">REJECTED ` + strconv.Itoa(entry.Score) + `/` + strconv.Itoa(entry.MinScore) + `</span>`
		}
		exempt := ""
		if entry.Exempt {
			exempt = ` <span class="tag mute">豁免</span>`
		}
		actions := `<span class="tag mute">未处理</span>`
		if len(entry.Actions) > 0 {
			actions = ""
			for _, action := range entry.Actions {
				class := "mute"
				switch action {
				case "MODEL_MAPPED":
					class = "ok"
				case "EFFORT_DOWNGRADED":
					class = "warn"
				}
				actions += `<span class="tag ` + class + `">` + html.EscapeString(action) + `</span>`
			}
		}
		fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="mono">%s%s</td><td>%s<br><span class="tag mute">%s</span></td><td class="mono">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><span class="tag %s">%s</span></td></tr>`,
			html.EscapeString(entry.Time), html.EscapeString(entry.APIKey), exempt,
			html.EscapeString(truncate(entry.Client, 26)), html.EscapeString(orDash(entry.Protocol)),
			html.EscapeString(entry.Model), routed, effort, admission, actions,
			resultClass(entry.Result), html.EscapeString(entry.Result))
	}
	b.WriteString(`</tbody></table>`)
	last := entries[len(entries)-1]
	b.WriteString(`<div class="note">最近一条请求命中的客户端特征（用于排查为什么没放行）：`)
	b.WriteString(html.EscapeString(orDash(strings.Join(last.Signals, ", "))))
	b.WriteString(`</div>`)
	return b.String() + "</body></html>"
}

func renderConfigPage() string {
	payload := effectiveConfigPayload()
	fields, _ := payload["fields"].([]map[string]any)
	var b strings.Builder
	b.WriteString(pageHeader("codex-policy · 配置说明", "插件 v"+pluginVersion+" · 在 config.yaml 的 plugins.configs.codex-policy 下修改，重启生效"))
	b.WriteString(`<div class="note">在线保存会改写 <span class="mono">config.yaml</span> 的 <span class="mono">plugins.configs.codex-policy</span> 段（先备份为 <span class="mono">config.yaml.bak-before-codex-policy</span>，再原子替换），并立即在插件内生效。
标注「需重启」的项由宿主读取，改完要重启容器。保存接口走宿主管理认证：
<span class="mono">POST /v0/management/plugins/codex-policy/settings</span>。
该接口只替换插件自己的那一段，<span class="mono">config.yaml</span> 其余部分的注释与格式保持不变。</div>`)
	b.WriteString(`<form id="cfg"><table><thead><tr><th class="k">配置项</th><th>状态</th><th>默认值</th><th>新值</th><th>说明</th></tr></thead><tbody>`)
	for _, field := range fields {
		key, _ := field["key"].(string)
		kind, _ := field["type"].(string)
		description, _ := field["description"].(string)
		fallback, _ := field["default"].(string)
		defaultCell := fallback
		if rendered := renderValue(field["default_raw"]); rendered != "-" {
			defaultCell = rendered
		}
		isDefault := sameValue(field["current"], field["default_raw"])
		stateTag := `<span class="tag mute">默认</span>`
		if !isDefault {
			stateTag = `<span class="tag warn">已修改</span>`
		}
		needsRestart := ""
		if key == "enabled" || key == "priority" {
			needsRestart = ` <span class="tag warn">需重启</span>`
		}
		fmt.Fprintf(&b, `<tr><td class="k mono">%s</td><td><span class="tag mute">%s</span></td><td class="mono">%s</td><td class="mono">%s</td><td>%s%s</td></tr>`,
			html.EscapeString(key), stateTag,
			html.EscapeString(orDash(defaultCell)), configInput(key, kind, field["current"], isDefault),
			needsRestart,
			strings.ReplaceAll(html.EscapeString(description), "\n", "<br>"))
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<div style="margin-top:14px"><input id="mgmtkey" placeholder="管理密钥（浏览器已登录则留空）" style="padding:6px 10px;border:1px solid #d6d9dd;border-radius:4px;width:320px">
	<button type="button" id="save" style="padding:6px 18px;border:0;border-radius:4px;background:#1a56c4;color:#fff;cursor:pointer">保存</button>
	<span id="hint"></span></div></form>`)
	raw, _ := payload["effective_yaml"].(string)
	b.WriteString(`<h2>当前生效配置（YAML）</h2><div class="empty"><pre class="mono" style="text-align:left;white-space:pre-wrap;margin:0">`)
	b.WriteString(html.EscapeString(strings.TrimSpace(raw)))
	b.WriteString(`</pre></div>`)
	b.WriteString(configScript)
	return b.String() + "</body></html>"
}

// configInput renders the editor for one config key. Values are carried in
// data attributes so the page stays a pure server-side render with no
// templating on the client.
func configInput(key, kind string, current any, isDefault bool) string {
	value := renderValue(current)
	if value == "-" {
		value = ""
	}
	attrs := ` data-key="` + html.EscapeString(key) + `" data-type="` + html.EscapeString(kind) + `"`
	style := ` style="padding:5px 8px;border:1px solid #d6d9dd;border-radius:4px;width:100%;min-width:220px"`
	dirty := ""
	if !isDefault {
		dirty = ` style="background:#fff8e6"`
	}
	switch kind {
	case "boolean":
		checked := ""
		if value == "true" {
			checked = " checked"
		}
		return `<input type="checkbox"` + attrs + dirty + checked + `>`
	case "integer":
		return `<input type="number" value="` + html.EscapeString(value) + `"` + attrs + dirty + style + `>`
	case "enum":
		options := ""
		for _, choice := range []string{"table", "line"} {
			selected := ""
			if choice == value {
				selected = " selected"
			}
			options += `<option value="` + choice + `"` + selected + `>` + choice + `</option>`
		}
		return `<select` + attrs + dirty + style + `>` + options + `</select>`
	case "array":
		if key == "model_map.rules" {
			return `<textarea rows="6" cols="52"` + attrs + dirty + ` style="padding:5px 8px;border:1px solid #d6d9dd;border-radius:4px;width:100%;font-family:ui-monospace,Menlo,monospace;font-size:12px">` + html.EscapeString(arrayAsYAML(current)) + `</textarea>`
		}
		flat := ""
		if items, ok := current.([]any); ok {
			parts := make([]string, 0, len(items))
			for _, item := range items {
				parts = append(parts, fmt.Sprint(item))
			}
			flat = strings.Join(parts, ", ")
		}
		return `<input type="text" value="` + html.EscapeString(flat) + `"` + attrs + dirty + style + `>`
	case "object":
		return `<span class="tag mute">` + html.EscapeString(value) + `</span>`
	default:
		return `<input type="text" value="` + html.EscapeString(value) + `"` + attrs + dirty + style + `>`
	}
}

func arrayAsYAML(value any) string {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- ")
		switch typed := item.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for i, key := range keys {
				if i > 0 {
					b.WriteString("  ")
				}
				b.WriteString(key + ": " + yamlScalar(typed[key]))
				b.WriteString("\n")
			}
		default:
			b.WriteString(yamlScalar(item))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
