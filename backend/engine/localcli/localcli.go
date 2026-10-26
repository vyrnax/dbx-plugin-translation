// Package localcli 实现「本地 CLI 代理」翻译引擎。
//
// # 为什么需要这个引擎
//
// 用户可能已经在本地装好了 claude / codex 这类 CLI 并完成了登录，
// 此时 CLI 自己持有凭据（OAuth token、订阅额度），用户【没有】也【不需要】
// 单独的 API Key。让侧车直接调用这些 CLI 做翻译，就能复用已有的登录态，
// 不必再去申请一把 Key。
//
// 这也是 DBX 自己的 AI 集成方式 —— 它的 ai_configs 表里 provider 可以是
// `claude-code-cli` / `codex-cli`，此时 apiKey 与 endpoint 都是空的。
//
// # 安全约束（本包最重要的部分）
//
// 调用外部进程的风险远高于发 HTTP 请求，因此有几条硬约束：
//
//  1. 【绝不】用 shell 拼接命令。一律 exec.CommandContext(name, args...)，
//     参数以数组形式传递，由操作系统直接 execve，不经过 shell 解析。
//     这样即使 extraArgs 里含 `; rm -rf /` 或 `$(whoami)`，
//     也只会被当成一个字面参数传给 CLI，不会被解释执行。
//
//  2. extraArgs 逐条校验，拒绝含 shell 元字符的项。这是纵深防御 ——
//     第 1 条已经保证了安全，但显式拒绝能让用户立刻发现配置写错了，
//     而不是困惑「为什么我的参数没生效」。
//
//  3. stdout 与 stderr 严格分离。stderr 只进 trace 日志，【绝不】混入译文。
//     CLI 经常往 stderr 打进度条、警告、调试信息，混进译文会毁掉结果。
//
//  4. 超时用 context 控制并 kill 进程。CLI 卡住时不能让整个侧车挂死。
//
//  5. 密钥字段为空是【正常】的（CLI 自持凭据），因此 NeedsSecret=false。
package localcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vyrnax/translation/engine"
	"github.com/vyrnax/translation/lang"
	"github.com/vyrnax/translation/trace"
)

// Kind 是本引擎的类型标识。
const Kind = "localcli"

// defaultMaxChars 是单次请求的字符上限。
//
// 命令行参数长度受操作系统限制（Linux 通常 128 KiB，Windows 32 KiB），
// 但我们是把文本通过 stdin 传给 CLI 的，不受该限制。
// 20000 字符对大模型上下文来说也很安全。
const defaultMaxChars = 20000

// defaultTimeoutSecs 是默认超时。
//
// 比 HTTP 引擎的 60 秒长一倍：CLI 冷启动（加载配置、建立会话、
// 可能还要走 OAuth 刷新）本身就要几秒到十几秒，加上模型推理时间，
// 60 秒经常不够。
const defaultTimeoutSecs = 120

// 支持的 CLI 预设。每个预设定义了如何构造命令行参数。
const (
	presetClaude = "claude"
	presetCodex  = "codex"
	presetCustom = "custom"
)

func init() {
	engine.Register(Kind, build, schema())
}

type engineImpl struct {
	preset   string
	cliPath  string   // 解析后的可执行文件绝对路径（或 PATH 里的命令名）
	baseArgs []string // 预设的固定参数
	extra    []string // 用户自定义的额外参数
	workDir  string
	timeout  time.Duration
	maxChars int
	prompt   string // 自定义 Prompt 模板（空则用内置）
	name     string
}

func build(cfg engine.Config, _ engine.Secrets, opts engine.Options) (engine.Engine, error) {
	preset := engine.ConfigString(cfg, "preset", presetClaude)
	switch preset {
	case presetClaude, presetCodex, presetCustom:
	default:
		return nil, engine.Newf(engine.KindConfig,
			"未知的 CLI 类型：%s（只支持 claude / codex / custom）", preset)
	}

	// 解析可执行文件路径
	cliPath, err := resolveCLIPath(engine.ConfigString(cfg, "cliPath", ""), preset)
	if err != nil {
		return nil, err
	}

	// 解析额外参数（逐条校验，拒绝 shell 元字符）
	extra, err := parseExtraArgs(engine.ConfigString(cfg, "extraArgs", ""))
	if err != nil {
		return nil, err
	}

	// 工作目录：留空用临时目录。
	//
	// 为什么不用侧车的当前目录：CLI 可能会在工作目录里创建文件
	// （会话记录、缓存、日志），落在插件目录里会污染数据目录，
	// 而且用户可能没意识到。临时目录是更安全的选择。
	workDir := strings.TrimSpace(engine.ConfigString(cfg, "workDir", ""))
	if workDir == "" {
		workDir = os.TempDir()
	} else {
		abs, err := filepath.Abs(workDir)
		if err != nil {
			return nil, engine.Wrap(engine.KindConfig, "工作目录路径非法", err)
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return nil, engine.Newf(engine.KindConfig,
				"工作目录不存在或不是目录：%s", abs)
		}
		workDir = abs
	}

	timeoutSecs := engine.ConfigInt(cfg, "timeoutSecs", defaultTimeoutSecs, 10, 600)

	return &engineImpl{
		preset:   preset,
		cliPath:  cliPath,
		baseArgs: baseArgsFor(preset),
		extra:    extra,
		workDir:  workDir,
		timeout:  time.Duration(timeoutSecs) * time.Second,
		maxChars: engine.ConfigInt(cfg, "maxChars", defaultMaxChars, 500, 100000),
		prompt:   engine.ConfigString(cfg, "promptTemplate", ""),
		name:     opts.Name,
	}, nil
}

// resolveCLIPath 解析 CLI 可执行文件路径。
//
// 留空时按预设名从 PATH 查找；填写时校验文件确实存在且可执行。
//
// 提前校验的价值：如果等到翻译时才发现「命令不存在」，
// 用户看到的是一次失败的翻译；在保存配置时就报错，
// 用户能立刻知道是路径写错了。
func resolveCLIPath(configured, preset string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		// 从 PATH 查找预设对应的命令名
		name := preset
		if preset == presetCustom {
			return "", engine.New(engine.KindConfig,
				"自定义 CLI 类型必须填写「CLI 路径」")
		}
		found, err := exec.LookPath(name)
		if err != nil {
			return "", engine.Newf(engine.KindConfig,
				"在 PATH 中找不到 %s 命令。请安装它，或在「CLI 路径」里填写完整路径", name)
		}
		return found, nil
	}

	// 用户填了路径：展开 ~ 并校验存在性
	path := configured
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	// 如果填的是纯命令名（不含路径分隔符），走 PATH 查找
	if !strings.ContainsAny(path, `/\`) {
		found, err := exec.LookPath(path)
		if err != nil {
			return "", engine.Newf(engine.KindConfig,
				"在 PATH 中找不到命令 %s", path)
		}
		return found, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", engine.Wrap(engine.KindConfig, "CLI 路径非法", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", engine.Newf(engine.KindConfig,
			"CLI 路径不存在：%s", abs)
	}
	if st.IsDir() {
		return "", engine.Newf(engine.KindConfig,
			"CLI 路径是一个目录而不是可执行文件：%s", abs)
	}
	return abs, nil
}

// baseArgsFor 返回各预设的固定参数。
//
// claude：-p（print 模式，非交互）+ --output-format stream-json（逐行 JSON，
//
//	便于增量提取文本）+ --verbose（stream-json 模式要求配合 verbose）
//
// codex：exec 子命令（非交互执行）
// custom：无固定参数，完全由用户的 extraArgs 决定
func baseArgsFor(preset string) []string {
	switch preset {
	case presetClaude:
		return []string{"-p", "--output-format", "stream-json", "--verbose"}
	case presetCodex:
		return []string{"exec", "--skip-git-repo-check"}
	default:
		return nil
	}
}

// shellMetaChars 是会被 shell 解释的元字符。
//
// 虽然我们【不经过 shell】执行（exec.Command 直接 execve），
// 这些字符本身无害，但显式拒绝有两个好处：
//  1. 用户误以为需要 shell 转义而写了 `\"` 之类，能立刻得到提示
//  2. 万一将来有人改成 shell 调用，这道校验能挡住注入
const shellMetaChars = ";|&$`<>(){}[]!#~*?\\"

// parseExtraArgs 按行分割额外参数并逐条校验。
//
// 用【行】而不是空格分割：参数值里可能含空格
// （例如 --append-system-prompt 的值是一整句话），
// 按空格分会把它切碎。一行一个参数更符合直觉。
func parseExtraArgs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for i, line := range strings.Split(raw, "\n") {
		arg := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if arg == "" || strings.HasPrefix(arg, "#") {
			continue // 空行与注释行跳过
		}
		if strings.ContainsAny(arg, shellMetaChars) {
			bad := ""
			for _, c := range arg {
				if strings.ContainsRune(shellMetaChars, c) {
					bad = string(c)
					break
				}
			}
			return nil, engine.Newf(engine.KindConfig,
				"额外参数第 %d 行含 shell 元字符 %q：%s\n"+
					"参数以数组形式直接传给进程、不经过 shell，因此不需要也不允许转义。"+
					"请去掉该字符后重试", i+1, bad, arg)
		}
		if len(arg) > 4096 {
			return nil, engine.Newf(engine.KindConfig,
				"额外参数第 %d 行过长（%d 字符，上限 4096）", i+1, len(arg))
		}
		out = append(out, arg)
	}
	return out, nil
}

func (e *engineImpl) Kind() string             { return Kind }
func (e *engineImpl) SupportsStreaming() bool  { return true }
func (e *engineImpl) MaxChars() int            { return e.maxChars }
func (e *engineImpl) SupportedLangs() []string { return lang.SupportedLangs(Kind) }

// ---------------------------------------------------------------------------
// Prompt 构造
// ---------------------------------------------------------------------------

// defaultSystemPrompt 是内置的翻译提示词。
//
// 与 openai 适配器的提示词内容一致，但措辞针对「CLI 单次调用」调整：
// CLI 没有 system/user 消息的区分（-p 模式只接受一段文本），
// 所以指令与正文要写在同一段里，并用明确的分隔标记隔开，
// 避免模型把指令本身也当成待翻译内容。
const defaultSystemPrompt = `你是一个专业的翻译引擎。请把下面分隔线之后的文本翻译成{targetLang}。

严格遵守：
1. 只输出译文本身，不要输出任何解释、说明、前言或结语。
2. 不要给译文加引号或任何包裹符号。
3. 完整保留原文的换行、缩进、空行与 Markdown 标记。
4. 完整保留占位符（如 {name}、%s、%d、${var}）与代码标识符，不要翻译它们。
5. 代码块与行内代码的内容保持原样，不要翻译。
6. 专有名词、产品名、API 名称保持原文。
7. 如果原文已经是{targetLang}，直接原样输出。

===== 待翻译文本开始 =====
{text}
===== 待翻译文本结束 =====`

// buildPrompt 构造传给 CLI 的完整提示词。
func (e *engineImpl) buildPrompt(r engine.Request) string {
	targetName := lang.LLMName(r.TargetLang)
	sourceName := lang.LLMName(r.SourceLang)

	tmpl := strings.TrimSpace(e.prompt)
	if tmpl == "" {
		tmpl = defaultSystemPrompt
	}
	// 用 strings.Replacer 而非 fmt.Sprintf：用户模板里可能出现任意 % 字符
	// （例如翻译含百分号的文本），Sprintf 会报 "unknown verb"。
	rep := strings.NewReplacer(
		"{targetLang}", targetName,
		"{sourceLang}", sourceName,
		"{text}", r.Source,
	)
	out := rep.Replace(tmpl)
	// 用户自定义模板若不含 {text}，说明他想自己控制正文位置，
	// 此时把原文追加在后面，保证正文一定被送到模型。
	if !strings.Contains(tmpl, "{text}") {
		out += "\n\n" + r.Source
	}
	return out
}

// ---------------------------------------------------------------------------
// 进程执行
// ---------------------------------------------------------------------------

// runCLI 启动 CLI 进程，把 prompt 通过 stdin 传入，逐行读取 stdout。
//
// 为什么用 stdin 而不是命令行参数传文本：
//   - 命令行参数长度受操作系统限制（Windows 约 32 KiB）
//   - 参数会出现在 `ps` / 任务管理器的输出里，翻译内容可能含敏感信息
//   - stdin 没有这两个问题
//
// emit 非 nil 时逐行推送增量（流式）；为 nil 时只在内部累积。
// scanResult 是读取 goroutine 回传给主流程的结果。
type scanResult struct {
	text string // 累积的完整输出
	err  error  // 读取过程中的错误（scanner.Err()）
}

func (e *engineImpl) runCLI(ctx context.Context, prompt string, emit func(string) error) (string, error) {
	// 用 WithTimeout 包一层：即使调用方的 ctx 没有超时，CLI 卡住也不能挂死侧车
	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// 【关键】exec.CommandContext 用参数数组，绝不拼接 shell 字符串。
	// 这样 extraArgs 里即使含 `; rm -rf /` 也只会被当成字面参数传给 CLI。
	args := make([]string, 0, len(e.baseArgs)+len(e.extra))
	args = append(args, e.baseArgs...)
	args = append(args, e.extra...)

	cmd := exec.CommandContext(runCtx, e.cliPath, args...)
	cmd.Dir = e.workDir
	cmd.Env = os.Environ()
	// 让子进程自成进程组，以便取消时能终止整棵树（见 proc_*.go）
	setProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", engine.Wrap(engine.KindConfig, "创建 stdin 管道失败", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", engine.Wrap(engine.KindConfig, "创建 stdout 管道失败", err)
	}
	// stderr 单独收集：CLI 常往 stderr 打进度条与警告，
	// 【绝不能】混入译文，但也不能丢弃（排查问题时需要）。
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return "", classifyStartErr(err, e.cliPath)
	}

	// 写入 prompt 后关闭 stdin，让 CLI 知道输入结束
	go func() {
		defer func() { _ = stdin.Close() }()
		_, _ = io.WriteString(stdin, prompt)
	}()

	// 【关键】读取必须放在独立 goroutine 里，主流程用 select 监听取消。
	//
	// 为什么不能直接在主流程里 for scanner.Scan()：
	//   Scan() 是阻塞读。如果 CLI 长时间不输出任何内容（例如在思考、
	//   在等网络、或者干脆是个 sleep），Scan 会一直阻塞，
	//   循环体里的 runCtx.Err() 检查【根本没有机会执行】。
	//   结果就是用户点了取消，却要等 CLI 自己跑完才返回。
	//
	//   本项目实测过这个 bug：取消一个 sleep 30 的假 CLI 耗时 29.4 秒。
	//
	// 放进 goroutine 后，主流程可以在 ctx.Done() 时立刻 killTree 并返回。
	results := make(chan scanResult, 1)
	go func() {
		var builder strings.Builder
		scanner := bufio.NewScanner(stdout)
		// CLI 的单行输出可能很长（stream-json 一行含完整消息），放大缓冲
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			text := e.extractText(line)
			if text == "" {
				continue
			}
			builder.WriteString(text)
			if emit != nil {
				if err := emit(text); err != nil {
					// emit 失败通常是取消传播：停止读取，让主流程收尾
					results <- scanResult{text: builder.String(), err: err}
					return
				}
			}
		}
		results <- scanResult{text: builder.String(), err: scanner.Err()}
	}()

	// 等待「读取完成」或「ctx 取消」，谁先到算谁
	var scan scanResult
	select {
	case scan = <-results:
		// 正常读完
	case <-runCtx.Done():
		// 取消或超时：终止整棵进程树，然后立刻返回。
		//
		// 必须杀整棵树而不是只杀直接子进程：Windows 上执行 .bat 会形成
		// cmd.exe → 实际命令 的结构，只杀 cmd.exe 会留下孤儿进程
		// 继续持有 stdout 管道写端，读取 goroutine 依然阻塞（泄漏）。
		killTree(cmd)
		// 排空 goroutine 的结果，避免它永久阻塞在 channel 发送上而泄漏。
		// 进程已被杀，管道会关闭，Scan 很快返回。
		go func() { <-results }()
		reason := runCtx.Err()
		if reason == context.DeadlineExceeded {
			return "", engine.New(engine.KindNetwork,
				"CLI 执行超时：本地 CLI 冷启动较慢，可在引擎配置里调大「超时（秒）」").
				WithDetail("preset=%s timeout=%s", e.preset, e.timeout)
		}
		return "", engine.Wrap(engine.KindCancelled, "翻译已取消", reason)
	}

	waitErr := cmd.Wait()
	stderrText := strings.TrimSpace(stderrBuf.String())
	if stderrText != "" {
		// stderr 只进日志，不进译文，也不进错误消息（可能含敏感路径）
		traceCLI(e.preset, stderrText)
	}

	// emit 返回的错误优先于进程退出码：那是前端主动取消的信号
	if scan.err != nil && emit != nil && !isScannerErr(scan.err) {
		return "", engine.Wrap(engine.KindCancelled, "翻译已取消", scan.err)
	}
	if scan.err != nil && isScannerErr(scan.err) {
		if runCtx.Err() != nil {
			return "", engine.Wrap(engine.KindCancelled, "翻译已取消", runCtx.Err())
		}
		return "", engine.Wrap(engine.KindProtocol, "读取 CLI 输出失败", scan.err)
	}

	if waitErr != nil {
		return "", classifyRunErr(waitErr, runCtx, e.preset, stderrText)
	}

	out := scan.text
	if strings.TrimSpace(out) == "" {
		msg := "CLI 没有返回任何内容"
		if stderrText != "" {
			msg += "（stderr 有输出，已记入 sidecar-trace.log）"
		}
		return "", engine.New(engine.KindProtocol, msg)
	}
	return out, nil
}

// isScannerErr 判断是否为 bufio.Scanner 自身的读取错误。
//
// 需要区分两类错误：
//   - scanner 读取错误（管道被关闭、行超长）→ 协议问题
//   - emit 回调返回的错误（前端取消）→ 取消，不算失败
//
// 混淆两者会让「用户主动取消」被报成「CLI 输出异常」，
// 用户会以为插件坏了而反复重试。
func isScannerErr(err error) bool {
	if err == nil {
		return false
	}
	// emit 的错误是我们自己包装的取消语义，不含 scanner 的特征
	msg := err.Error()
	if strings.Contains(msg, "token too long") {
		return true
	}
	// io.EOF 是正常结束，不算错误
	if err == io.EOF {
		return false
	}
	// 其余的读取错误（管道关闭等）归为 scanner 错误
	return !strings.Contains(msg, "翻译已取消") && !strings.Contains(msg, "context canceled")
}

// extractText 从一行 CLI 输出里提取译文增量。
//
// 各预设的输出格式不同：
//   - claude --output-format stream-json：每行一个 JSON 对象，
//     文本增量在 message.content[].text 或 delta.text 里
//   - codex exec：纯文本行，直接就是输出
//   - custom：按纯文本处理（用户自己保证格式）
//
// 解析失败时【不报错】而是跳过该行：CLI 可能输出调试信息、
// 进度条、空行，因为一行解析失败就放弃整个翻译不值得。
func (e *engineImpl) extractText(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}

	// 非 JSON 行：claude 的 stream-json 模式偶尔会混入纯文本，
	// codex/custom 则全是纯文本。直接当译文处理。
	if trimmed[0] != '{' && trimmed[0] != '[' {
		// 但要过滤掉明显的非译文行（进度条、日志前缀）
		if isNoiseLine(trimmed) {
			return ""
		}
		return trimmed
	}

	// JSON 行：尝试多种已知的字段位置
	var probe struct {
		Type string `json:"type"`
		// claude stream-json: {"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		// 增量形态: {"type":"content_block_delta","delta":{"text":"..."}}
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
		// codex exec --json 形态: {"msg":{"text":"..."}} 或 {"text":"..."}
		Msg struct {
			Text string `json:"text"`
		} `json:"msg"`
		Text   string `json:"text"`
		Result string `json:"result"`
		// 错误形态: {"type":"error","error":{...}} 或 {"is_error":true}
		IsError bool `json:"is_error"`
		Error   *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return "" // 解析失败，跳过
	}

	// 错误事件：不提取文本，交给调用方通过 Wait 的错误处理
	if probe.IsError || probe.Error != nil {
		return ""
	}
	// 系统初始化事件（claude 的 {"type":"system","subtype":"init"}）不含译文
	if probe.Type == "system" || probe.Type == "result" {
		// result 事件里可能带完整结果，作为兜底
		if probe.Type == "result" && probe.Result != "" {
			return ""
		}
		return ""
	}

	// 按优先级提取：delta.text → message.content[].text → msg.text → text
	if probe.Delta.Text != "" {
		return probe.Delta.Text
	}
	var sb strings.Builder
	for _, c := range probe.Message.Content {
		if c.Type == "text" || c.Type == "" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() > 0 {
		return sb.String()
	}
	if probe.Msg.Text != "" {
		return probe.Msg.Text
	}
	if probe.Text != "" {
		return probe.Text
	}
	return ""
}

// isNoiseLine 过滤明显的非译文行。
//
// CLI 在 stderr 之外也可能往 stdout 打进度信息。
// 这里只过滤【高置信度】的噪音（进度条、百分比、时间戳前缀），
// 宁可漏过也不要误删真正的译文 —— 误删译文比多几个字符严重得多。
func isNoiseLine(line string) bool {
	// 进度条：含大量重复的填充字符
	if strings.Count(line, "=") > 20 || strings.Count(line, "#") > 20 {
		return true
	}
	// 纯百分比行
	if len(line) <= 6 && strings.HasSuffix(line, "%") {
		return true
	}
	return false
}

// classifyStartErr 归类进程启动失败。
//
// 启动失败与「启动了但翻译失败」是完全不同的问题，
// 必须区分：前者是配置问题（路径错、没权限），后者可能是网络或额度问题。
func classifyStartErr(err error, cliPath string) error {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"), strings.Contains(lower, "no such file"),
		strings.Contains(lower, "cannot find"):
		return engine.Wrap(engine.KindConfig,
			"找不到 CLI 可执行文件，请检查「CLI 路径」配置", err).
			WithDetail("路径：%s", cliPath)
	case strings.Contains(lower, "permission denied"), strings.Contains(lower, "access is denied"):
		return engine.Wrap(engine.KindConfig,
			"没有执行权限，请检查文件权限（macOS/Linux 需要 chmod +x）", err).
			WithDetail("路径：%s", cliPath)
	case strings.Contains(lower, "is a directory"):
		return engine.Wrap(engine.KindConfig,
			"CLI 路径指向的是一个目录，不是可执行文件", err).
			WithDetail("路径：%s", cliPath)
	}
	return engine.Wrap(engine.KindConfig, "启动 CLI 进程失败", err).
		WithDetail("路径：%s", cliPath)
}

// classifyRunErr 归类进程运行失败。
func classifyRunErr(waitErr error, runCtx context.Context, preset, stderrText string) error {
	// 超时：context deadline exceeded
	if runCtx.Err() == context.DeadlineExceeded {
		return engine.New(engine.KindNetwork,
			"CLI 执行超时：本地 CLI 冷启动较慢，可在引擎配置里调大「超时（秒）」").
			WithDetail("preset=%s", preset)
	}
	// 用户取消
	if runCtx.Err() == context.Canceled {
		return engine.Wrap(engine.KindCancelled, "翻译已取消", waitErr)
	}

	// 从 stderr 里提取有用的错误线索（截断，且不含密钥）
	hint := extractCLIHint(stderrText)

	// ExitError 能拿到退出码
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		base := fmt.Sprintf("CLI 执行失败（退出码 %d）", code)
		if hint != "" {
			base += "：" + hint
		}
		// 常见退出码的语义提示
		switch {
		case strings.Contains(strings.ToLower(hint), "unauthorized"),
			strings.Contains(strings.ToLower(hint), "not logged in"),
			strings.Contains(strings.ToLower(hint), "authentication"),
			strings.Contains(strings.ToLower(hint), "invalid api key"):
			return engine.New(engine.KindAuth, base+"。请先在终端里登录该 CLI")
		case strings.Contains(strings.ToLower(hint), "rate limit"),
			strings.Contains(strings.ToLower(hint), "quota"),
			strings.Contains(strings.ToLower(hint), "too many requests"):
			return engine.New(engine.KindRateLimit, base+"。请稍后再试")
		}
		return engine.New(engine.KindUpstream, base)
	}
	return engine.Wrap(engine.KindUpstream, "CLI 执行失败", waitErr)
}

// extractCLIHint 从 stderr 里提取最后一行有意义的错误信息。
//
// CLI 的 stderr 往往有几十行（进度、调试信息），真正的原因通常在最后。
// 只取最后几行里的非空行，并截断到 200 字符。
func extractCLIHint(stderrText string) string {
	if strings.TrimSpace(stderrText) == "" {
		return ""
	}
	lines := strings.Split(stderrText, "\n")
	// 从后往前找第一条有实质内容的行
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-8; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isNoiseLine(line) {
			continue
		}
		// 跳过纯进度类输出
		if strings.HasPrefix(line, "[") && strings.Contains(line, "%") {
			continue
		}
		return engine.TruncateRunes(line, 200)
	}
	return engine.TruncateRunes(strings.TrimSpace(stderrText), 200)
}

// traceCLI 把 CLI 的 stderr 写进侧车追踪日志。
//
// 不写进错误消息（可能含本地路径等敏感信息），但排查问题时是关键线索。
// trace 包自带脱敏，密钥类字符串会被自动掩码。
//
// 直接导入 trace 包是安全的：trace 只依赖标准库（fmt/os/regexp/strings/sync/time），
// 不导入本项目任何包，因此不存在循环依赖。
func traceCLI(preset, stderrText string) {
	// 只记前 2000 字符，避免日志被 CLI 的冗长输出撑爆
	trace.Log("localcli(%s) stderr: %s", preset, engine.TruncateRunes(stderrText, 2000))
}

// ---------------------------------------------------------------------------
// Engine 接口实现
// ---------------------------------------------------------------------------

func (e *engineImpl) Translate(ctx context.Context, r engine.Request) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()
	out, err := e.runCLI(ctx, e.buildPrompt(r), nil)
	if err != nil {
		return engine.Result{}, err
	}
	// CLI 背后也是大模型，同样会加「译文：」前缀或包裹引号，需要清洗
	target := engine.CleanTranslation(out)
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"CLI 返回的内容清洗后为空（可能全是解释性文字）")
	}
	return engine.Result{
		Target:    target,
		ElapsedMs: time.Since(start).Milliseconds(),
		Chunks:    1,
		Engine:    engine.DisplayName(e.name, e.preset+" CLI"),
		Model:     e.preset,
	}, nil
}

func (e *engineImpl) Stream(ctx context.Context, r engine.Request, emit func(engine.Chunk) error) (engine.Result, error) {
	if err := engine.ValidateRequest(r); err != nil {
		return engine.Result{}, err
	}
	start := time.Now()
	index := 0
	out, err := e.runCLI(ctx, e.buildPrompt(r), func(text string) error {
		chunk := engine.Chunk{Text: text, Index: index}
		index++
		return emit(chunk)
	})
	if err != nil {
		return engine.Result{}, err
	}
	target := engine.CleanTranslation(out)
	if strings.TrimSpace(target) == "" {
		return engine.Result{}, engine.New(engine.KindProtocol,
			"CLI 返回的内容清洗后为空（可能全是解释性文字）")
	}
	return engine.Result{
		Target:    target,
		ElapsedMs: time.Since(start).Milliseconds(),
		Chunks:    index,
		Engine:    engine.DisplayName(e.name, e.preset+" CLI"),
		Model:     e.preset,
	}, nil
}

func (e *engineImpl) Test(ctx context.Context) error {
	// 探测用最短文本：目的是验证「CLI 存在 + 能启动 + 已登录 + 能出结果」
	_, err := e.Translate(ctx, engine.Request{
		Source:     "hello",
		SourceLang: "en",
		TargetLang: "zh",
	})
	return err
}
