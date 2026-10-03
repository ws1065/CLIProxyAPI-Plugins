package main

import "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

// configFieldDoc is the single source of truth for a configuration key.
// The same table feeds the management panel field list and the "/config"
// page, so a documented default can never drift from what defaultConfig()
// actually applies.
type configFieldDoc struct {
	Key         string
	Type        pluginapi.ConfigFieldType
	Default     string
	Enum        []string
	Description string
}

var configFieldDocs = []configFieldDoc{
	{
		Key: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "总开关。false 时宿主直接跳过加载插件且不打印任何日志。\n排查“插件消失”先看这里。注意：宿主把缺省值当作 false，所以这一项必须显式写 true 才会加载。",
	},
	{
		Key: "priority", Type: pluginapi.ConfigFieldTypeInteger, Default: "0",
		Description: "插件优先级，数值越小越先执行。宿主在配置缺省时按 0 处理。",
	},
	{
		Key: "enforce_official_client", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "是否拒绝非官方 Codex 客户端。false = 只记录不拦截。",
	},
	{
		Key: "reject_status", Type: pluginapi.ConfigFieldTypeInteger, Default: "403",
		Description: "拒绝时返回的 HTTP 状态码，非法值回退为 403。",
	},
	{
		Key: "reject_message", Type: pluginapi.ConfigFieldTypeString, Default: "客户端不支持访问",
		Description: "拒绝时返回的错误信息。",
	},
	{
		Key: "official_min_score", Type: pluginapi.ConfigFieldTypeInteger, Default: "5",
		Description: "准入分数阈值（1-20）。必须先命中至少一个强信号，再总分达标才放行。\n调高更严，调低更松；低于 1 会被纠正为 1。",
	},
	{
		Key: "apply_to_non_codex", Type: pluginapi.ConfigFieldTypeBoolean, Default: "false",
		Description: "模型映射 / 思考降级是否也对 Claude / Gemini / OpenAI chat 协议生效。\nfalse = 只改写 codex/responses 流量。客户端准入检查不受此项影响，见 enforce_all_protocols。",
	},
	{
		Key: "enforce_all_protocols", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "官方客户端准入检查是否覆盖所有协议。\ntrue = /v1/chat/completions 等非 Responses 接口同样拦截非官方客户端（推荐，符合“只允许官方客户端”的需求）。\nfalse = 退回旧行为，只在 codex/responses 接口上拦截，其他协议完全放行。",
	},
	{
		Key: "exempt_api_keys", Type: pluginapi.ConfigFieldTypeArray, Default: "[]",
		Description: "豁免名单（客户端 access key）。命中的 key 既跳过准入，也不做模型映射和思考降级。\n以明文存储，请只填本服务对外的 access key，不要填上游 provider 的 key。常量时间比较。",
	},
	{
		Key: "exempt_bypass_admission", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "豁免 key 是否跳过官方客户端准入检查。",
	},
	{
		Key: "effort.enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "是否启用思考等级降级。",
	},
	{
		Key: "effort.downgrade_from", Type: pluginapi.ConfigFieldTypeArray, Default: "[xhigh, max, ultra]",
		Description: "触发降级的思考等级列表（大小写不敏感）。",
	},
	{
		Key: "effort.target", Type: pluginapi.ConfigFieldTypeString, Default: "medium",
		Description: "降级后的目标思考等级。",
	},
	{
		Key: "effort.models", Type: pluginapi.ConfigFieldTypeArray, Default: "[]",
		Description: "只对这些模型降级，支持 * 通配（如 gpt-5.6-*）。空 = 不限模型。",
	},
	{
		Key: "effort.skip_for_exempt_keys", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "豁免 key 是否跳过思考等级降级。",
	},
	{
		Key: "model_map.enabled", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "是否启用模型映射（客户端无感转发）。",
	},
	{
		Key: "model_map.provider", Type: pluginapi.ConfigFieldTypeString, Default: "（空 = 自动选择）",
		Description: "默认留空或填 auto：由 CPA 根据规则的目标模型，自动选择当前可用供应商，沿用宿主的凭据调度和失败重试。更换渠道不用修改映射规则。\n可选：填写 provider key 固定渠道，如 openai-compatible-subapi。显式渠道不可用时记录 PROVIDER_UNAVAILABLE / ROUTE_REJECTED。",
	},
	{
		Key: "model_map.rules", Type: pluginapi.ConfigFieldTypeArray, Default: "[]",
		Description: "映射规则列表，按顺序匹配，命中第一条即生效。\n每条：from=客户端请求的模型（支持 * 通配）、to=目标模型、keys=可选限定 access key。\nprovider 通常省略：继承默认供应商设置；填 auto 可覆盖全局固定渠道，恢复自动选择。目标模型保持固定，不会自动替换成别的模型。自动模式支持推理请求，不支持 token count 接口。",
	},
	{
		Key: "model_map.preserve_thinking_suffix", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "请求模型带思考后缀时（如 model(high)）保留后缀，只替换主模型名。",
	},
	{
		Key: "model_map.rewrite_response_model", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "把响应（含流式 SSE）里的模型名改回客户端请求的名字，客户端因此看不到真实模型。\n关闭后客户端会看到上游真实模型名。",
	},
	{
		Key: "model_map.skip_for_exempt_keys", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "豁免 key 是否跳过模型映射。",
	},
	{
		Key: "probe_model", Type: pluginapi.ConfigFieldTypeString, Default: "（空）",
		Description: "零成本验证用。填入可路由的真实模型后，携带 probe_header 的请求会在本地返回策略报告而不调用上游。\n生产环境务必留空：若该模型同时承载真实流量且请求没带 probe 头，真实流式响应会被截断，表现为 stream closed before response.completed。",
	},
	{
		Key: "probe_header", Type: pluginapi.ConfigFieldTypeString, Default: "X-Policy-Probe",
		Description: "探针必须携带的请求头名。",
	},
	{
		Key: "probe_header_value", Type: pluginapi.ConfigFieldTypeString, Default: "1",
		Description: "探针必须携带的请求头值。",
	},
	{
		Key: "log_decisions", Type: pluginapi.ConfigFieldTypeBoolean, Default: "true",
		Description: "是否把每个请求的处理决策打印到容器日志（stderr）。",
	},
	{
		Key: "log_format", Type: pluginapi.ConfigFieldTypeEnum, Default: "table",
		Enum:        []string{"table", "line"},
		Description: "日志格式。table = 每个请求一张对齐的审计表；line = 单行 key=value，便于 grep。",
	},
	{
		Key: "log_buffer_size", Type: pluginapi.ConfigFieldTypeInteger, Default: "200",
		Description: "插件菜单“请求日志”页面保留的最近记录条数（1-2000，重启清空）。",
	},
	{
		Key: "config_path", Type: pluginapi.ConfigFieldTypeString, Default: "（自动探测）",
		Description: "在线保存时写入的 config.yaml 路径。留空=按 cwd 逐级探测。\n保存会先写 config.yaml.bak-before-codex-policy 备份，再原子替换；\n只有插件自己读取的配置项（准入/思考/映射/日志）会立即生效，\n由宿主读取的 enabled 等项仍需重启容器。",
	},
}

func configFields() []pluginapi.ConfigField {
	fields := make([]pluginapi.ConfigField, 0, len(configFieldDocs))
	for _, doc := range configFieldDocs {
		fields = append(fields, pluginapi.ConfigField{
			Name:        doc.Key,
			Type:        doc.Type,
			EnumValues:  doc.Enum,
			Description: doc.Description,
		})
	}
	return fields
}
