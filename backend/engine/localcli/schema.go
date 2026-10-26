package localcli

import "github.com/vyrnax/translation/engine"

// schema 返回本引擎的配置表单描述。
//
// 与其它引擎的关键差异：NeedsSecret = false。
// 本地 CLI 自己持有凭据（OAuth token、订阅额度），用户不需要也没有 API Key。
// 这正是这个引擎的价值 —— 复用已有的 CLI 登录态。
func schema() engine.Schema {
	return engine.Schema{
		Kind:          Kind,
		Label:         "本地 CLI 代理",
		LabelEn:       "Local CLI proxy",
		DefaultName:   "本地 CLI 翻译",
		DefaultNameEn: "Local CLI Translation",
		Description:   "调用本机已登录的 claude / codex 命令行工具做翻译，复用其登录态与订阅额度，无需单独申请 API Key。支持流式输出。",
		DescriptionEn: "Invokes a locally installed and logged-in claude / codex CLI to translate, reusing its credentials and subscription quota. No separate API key needed. Supports streaming.",
		Streaming:     true,
		MaxChars:      defaultMaxChars,
		NeedsSecret:   false, // CLI 自持凭据，不需要 API Key
		Fields: []engine.Field{
			{
				Key:      "preset",
				Label:    "CLI 类型",
				LabelEn:  "CLI type",
				Type:     engine.FieldSelect,
				Required: true,
				Default:  presetClaude,
				Options: []engine.Option{
					{Label: "Claude Code（claude -p）", Value: presetClaude},
					{Label: "Codex CLI（codex exec）", Value: presetCodex},
					{Label: "自定义命令", Value: presetCustom},
				},
				Description:   "决定如何构造命令行参数。Claude Code 用 -p --output-format stream-json；Codex 用 exec 子命令；自定义则完全由「额外参数」决定。",
				DescriptionEn: "Determines how command-line arguments are built. Claude Code uses -p --output-format stream-json; Codex uses the exec subcommand; custom relies entirely on Extra arguments.",
			},
			{
				Key:           "cliPath",
				Label:         "CLI 路径",
				LabelEn:       "CLI path",
				Type:          engine.FieldText,
				Placeholder:   "留空则从 PATH 查找 claude / codex",
				Description:   "可执行文件的路径。留空时按 CLI 类型从系统 PATH 查找；也可填命令名（如 claude）或完整路径（如 C:\\Users\\me\\AppData\\Roaming\\npm\\claude.cmd）。保存时会校验文件是否存在。",
				DescriptionEn: "Path to the executable. Leave empty to look up claude / codex on PATH; a bare command name or a full path also works. Existence is validated on save.",
			},
			{
				Key:           "extraArgs",
				Label:         "额外参数",
				LabelEn:       "Extra arguments",
				Type:          engine.FieldTextarea,
				Multiline:     3,
				Placeholder:   "--model claude-sonnet-4-5\n--append-system-prompt 你是专业译者",
				Description:   "每行一个参数（参数值含空格时不要加引号，整行就是一个参数）。以 # 开头的行是注释。参数以数组形式直接传给进程、不经过 shell，因此含 ; | & $ ` 等字符会被拒绝。",
				DescriptionEn: "One argument per line (no quotes needed even if the value contains spaces). Lines starting with # are comments. Arguments are passed as an array directly to the process, never through a shell, so ; | & $ ` and similar characters are rejected.",
			},
			{
				Key:           "workDir",
				Label:         "工作目录",
				LabelEn:       "Working directory",
				Type:          engine.FieldText,
				Placeholder:   "留空使用系统临时目录",
				Description:   "CLI 进程的工作目录。留空用系统临时目录（推荐）—— CLI 可能在此创建会话记录或缓存，落在插件数据目录里会造成污染。",
				DescriptionEn: "Working directory for the CLI process. Leave empty to use the system temp directory (recommended): CLIs may create session files or caches there, which would pollute the plugin data directory.",
			},
			engine.TimeoutField(defaultTimeoutSecs),
			engine.MaxCharsField(defaultMaxChars),
			engine.ConcurrencyField(1,
				"同时运行的 CLI 进程数。每个进程都会占用内存并消耗订阅额度，建议保持 1。",
				"Number of concurrent CLI processes. Each consumes memory and subscription quota, so 1 is recommended."),
			{
				Key:           "promptTemplate",
				Label:         "自定义 Prompt 模板",
				LabelEn:       "Custom prompt template",
				Type:          engine.FieldTextarea,
				Multiline:     5,
				Placeholder:   "留空使用内置模板。可用占位符：{targetLang} {sourceLang} {text}",
				Description:   "留空时使用内置翻译模板。支持占位符：{targetLang} 目标语言、{sourceLang} 源语言、{text} 待译文本。若模板不含 {text}，原文会自动追加到末尾。",
				DescriptionEn: "Leave empty to use the built-in template. Placeholders: {targetLang}, {sourceLang}, {text}. If the template has no {text}, the source text is appended automatically.",
			},
		},
	}
}
