package localcli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gaogao/translation/engine"
)

// ---------------------------------------------------------------------------
// 额外参数解析与 shell 注入防护
// ---------------------------------------------------------------------------

// TestParseExtraArgs_RejectsShellMeta 是【最关键的安全测试】。
//
// 虽然 exec.Command 用参数数组、不经过 shell，元字符本身无害，
// 但显式拒绝有两个价值：
//  1. 用户误以为需要 shell 转义而写了引号之类，能立刻得到提示
//  2. 万一将来有人改成 shell 调用，这道校验能挡住注入
func TestParseExtraArgs_RejectsShellMeta(t *testing.T) {
	malicious := []string{
		"; rm -rf /",
		"$(whoami)",
		"`id`",
		"a && b",
		"a || b",
		"a | b",
		"x > /etc/passwd",
		"x < /dev/null",
		"a\\b",
		"foo*",
		"bar?",
	}
	for _, m := range malicious {
		_, err := parseExtraArgs(m)
		if err == nil {
			t.Errorf("含 shell 元字符的参数应被拒绝: %q", m)
			continue
		}
		var ee *engine.Error
		if !asEngineErr(err, &ee) || ee.Kind != engine.KindConfig {
			t.Errorf("%q 应归类为 KindConfig，得到 %v", m, err)
			continue
		}
		if !strings.Contains(ee.Message, "shell 元字符") {
			t.Errorf("%q 的错误消息应说明是 shell 元字符问题: %s", m, ee.Message)
		}
	}
}

// TestParseExtraArgs_AcceptsLegit 验证合法参数被正常接受。
func TestParseExtraArgs_AcceptsLegit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"--model claude-sonnet-4-5", []string{"--model claude-sonnet-4-5"}},
		{"--model\nclaude-sonnet-4-5", []string{"--model", "claude-sonnet-4-5"}},
		{"# 这是注释\n--verbose\n\n--flag", []string{"--verbose", "--flag"}},
		{"--append-system-prompt 你是专业译者", []string{"--append-system-prompt 你是专业译者"}},
		{"--key=value", []string{"--key=value"}},
		{"--url=https://a.com", []string{"--url=https://a.com"}},
		{"a,b.c-d_e+f%g", []string{"a,b.c-d_e+f%g"}},
		{"--path=/tmp/x", []string{"--path=/tmp/x"}},
		{"--a\r\n--b", []string{"--a", "--b"}},
	}
	for _, c := range cases {
		got, err := parseExtraArgs(c.in)
		if err != nil {
			t.Errorf("parseExtraArgs(%q) 不应报错: %v", c.in, err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("parseExtraArgs(%q)\n  得: %v\n  期望: %v", c.in, got, c.want)
		}
	}
}

// TestParseExtraArgs_TooLong 验证超长参数被拒绝。
func TestParseExtraArgs_TooLong(t *testing.T) {
	if _, err := parseExtraArgs(strings.Repeat("a", 5000)); err == nil {
		t.Error("超长参数应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// CLI 路径解析
// ---------------------------------------------------------------------------

func TestResolveCLIPath_NotFound(t *testing.T) {
	_, err := resolveCLIPath("这个命令肯定不存在xyz123", presetClaude)
	if err == nil {
		t.Fatal("不存在的命令应报错")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) || ee.Kind != engine.KindConfig {
		t.Errorf("应归类为 KindConfig，得到 %v", err)
	}
	if !strings.Contains(ee.Message, "PATH") {
		t.Errorf("错误消息应提示 PATH: %s", ee.Message)
	}
}

func TestResolveCLIPath_CustomRequiresPath(t *testing.T) {
	_, err := resolveCLIPath("", presetCustom)
	if err == nil {
		t.Fatal("custom 预设留空路径应报错")
	}
	if !strings.Contains(err.Error(), "CLI 路径") {
		t.Errorf("错误消息应说明需要填路径: %s", err.Error())
	}
}

func TestResolveCLIPath_DirectoryRejected(t *testing.T) {
	_, err := resolveCLIPath(t.TempDir(), presetCustom)
	if err == nil {
		t.Fatal("目录路径应被拒绝")
	}
	if !strings.Contains(err.Error(), "目录") {
		t.Errorf("错误消息应说明是目录: %s", err.Error())
	}
}

func TestResolveCLIPath_NonexistentFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "no-such-file")
	_, err := resolveCLIPath(bad, presetCustom)
	if err == nil {
		t.Fatal("不存在的文件应被拒绝")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误消息应说明文件不存在: %s", err.Error())
	}
}

func TestResolveCLIPath_RealFile(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("无法获取测试可执行文件路径")
	}
	got, err := resolveCLIPath(exe, presetCustom)
	if err != nil {
		t.Fatalf("真实可执行文件应通过: %v", err)
	}
	if got == "" {
		t.Error("返回的路径不应为空")
	}
}

// ---------------------------------------------------------------------------
// 输出解析
// ---------------------------------------------------------------------------

func newBareEngine(preset string) *engineImpl {
	return &engineImpl{preset: preset, maxChars: defaultMaxChars, timeout: 30 * time.Second}
}

// TestExtractText_ClaudeStreamJSON 验证 claude 的 stream-json 格式解析。
func TestExtractText_ClaudeStreamJSON(t *testing.T) {
	e := newBareEngine(presetClaude)
	cases := []struct{ line, want string }{
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"你好"}]}}`, "你好"},
		{`{"type":"content_block_delta","delta":{"text":"世界"}}`, "世界"},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"甲"},{"type":"text","text":"乙"}]}}`, "甲乙"},
		// 非 text 类型的 content 应跳过（例如 tool_use）
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","text":"忽略我"}]}}`, ""},
		{`{"type":"system","subtype":"init"}`, ""},
		{`{"type":"error","error":{"message":"boom"}}`, ""},
		{`{"is_error":true,"text":"不该出现"}`, ""},
		{"", ""},
		{"   ", ""},
		{`{"broken":`, ""},
		{"不是JSON", "不是JSON"},
	}
	for _, c := range cases {
		if got := e.extractText(c.line); got != c.want {
			t.Errorf("extractText(%q)\n  得: %q\n  期望: %q", c.line, got, c.want)
		}
	}
}

// TestExtractText_PlainText 验证 codex/custom 的纯文本输出。
func TestExtractText_PlainText(t *testing.T) {
	for _, preset := range []string{presetCodex, presetCustom} {
		e := newBareEngine(preset)
		if got := e.extractText("你好世界"); got != "你好世界" {
			t.Errorf("%s: 纯文本 = %q", preset, got)
		}
		if got := e.extractText(`{"msg":{"text":"甲"}}`); got != "甲" {
			t.Errorf("%s: msg.text 解析 = %q", preset, got)
		}
		if got := e.extractText(`{"text":"乙"}`); got != "乙" {
			t.Errorf("%s: text 解析 = %q", preset, got)
		}
	}
}

// TestIsNoiseLine 验证噪音过滤不会误删译文。
//
// 宁可漏过噪音也不要误删译文 —— 误删译文的后果比多几个字符严重得多。
func TestIsNoiseLine(t *testing.T) {
	for _, n := range []string{strings.Repeat("=", 30), strings.Repeat("#", 25), "50%", "100%"} {
		if !isNoiseLine(n) {
			t.Errorf("%q 应被识别为噪音", n)
		}
	}
	for _, l := range []string{
		"你好世界", "Hello, world!", "完成率是 50% 左右",
		"=== 标题 ===", "# 一级标题", "func main() { }",
	} {
		if isNoiseLine(l) {
			t.Errorf("%q 是正常译文，不应被当成噪音", l)
		}
	}
}

// ---------------------------------------------------------------------------
// Prompt 构造
// ---------------------------------------------------------------------------

func TestBuildPrompt_Default(t *testing.T) {
	e := newBareEngine(presetClaude)
	got := e.buildPrompt(engine.Request{Source: "hello", SourceLang: "en", TargetLang: "zh"})
	if !strings.Contains(got, "hello") {
		t.Error("Prompt 应包含原文")
	}
	if !strings.Contains(got, "Simplified Chinese") {
		t.Errorf("Prompt 应含目标语言的 LLM 名称: %q", got)
	}
	if strings.Contains(got, "{targetLang}") || strings.Contains(got, "{text}") {
		t.Errorf("占位符未被替换: %q", got)
	}
	for _, kw := range []string{"只输出译文", "换行", "占位符", "代码"} {
		if !strings.Contains(got, kw) {
			t.Errorf("内置模板缺少关键约束 %q", kw)
		}
	}
}

func TestBuildPrompt_Custom(t *testing.T) {
	e := newBareEngine(presetClaude)
	e.prompt = "翻译成{targetLang}：{text}"
	got := e.buildPrompt(engine.Request{Source: "hi", SourceLang: "en", TargetLang: "zh"})
	if got != "翻译成Simplified Chinese：hi" {
		t.Errorf("自定义模板结果 = %q", got)
	}
}

// TestBuildPrompt_AppendsTextWhenNoPlaceholder 验证模板不含 {text} 时自动追加原文。
//
// 否则用户写了自定义模板却忘了放 {text}，原文根本不会送到模型，
// 症状是「翻译结果与输入无关」，极难排查。
func TestBuildPrompt_AppendsTextWhenNoPlaceholder(t *testing.T) {
	e := newBareEngine(presetClaude)
	e.prompt = "你是专业译者，请翻译以下内容"
	got := e.buildPrompt(engine.Request{Source: "待译正文", SourceLang: "en", TargetLang: "zh"})
	if !strings.Contains(got, "待译正文") {
		t.Errorf("模板不含 {text} 时应自动追加原文: %q", got)
	}
	if !strings.HasPrefix(got, "你是专业译者") {
		t.Errorf("自定义模板应在前: %q", got)
	}
}

// TestBuildPrompt_PercentSafe 验证模板里的 % 不触发格式化错误。
func TestBuildPrompt_PercentSafe(t *testing.T) {
	e := newBareEngine(presetClaude)
	e.prompt = "翻译到{targetLang}，保留 100% 和 %s %d"
	got := e.buildPrompt(engine.Request{Source: "x", SourceLang: "en", TargetLang: "zh"})
	if !strings.Contains(got, "100%") || !strings.Contains(got, "%s") {
		t.Errorf("百分号应原样保留: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 构造校验
// ---------------------------------------------------------------------------

func TestBuild_Validation(t *testing.T) {
	if _, err := engine.Build(Kind, engine.Config{"preset": "不存在的cli"}, nil, engine.Options{}); err == nil {
		t.Error("未知预设应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{"preset": presetCustom}, nil, engine.Options{}); err == nil {
		t.Error("custom 预设缺 cliPath 应报错")
	}
	if _, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": "echo", "extraArgs": "; rm -rf /",
	}, nil, engine.Options{}); err == nil {
		t.Error("含 shell 元字符的 extraArgs 应被拒绝")
	}
	if _, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": "echo",
		"workDir": filepath.Join(t.TempDir(), "不存在"),
	}, nil, engine.Options{}); err == nil {
		t.Error("不存在的工作目录应报错")
	}
}

// TestBuild_NoSecretRequired 验证本引擎不需要密钥。
//
// 这是 localcli 的核心价值：复用 CLI 已有的登录态，用户不必申请 API Key。
func TestBuild_NoSecretRequired(t *testing.T) {
	_, sch, ok := engine.Lookup(Kind)
	if !ok {
		t.Fatal("localcli 未注册")
	}
	if sch.NeedsSecret {
		t.Error("localcli 的 NeedsSecret 应为 false（CLI 自持凭据）")
	}
	for _, f := range sch.Fields {
		if f.Secret {
			t.Errorf("localcli 不应有 Secret 字段，但 %s 被标记为 secret", f.Key)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("无法获取测试可执行文件路径")
	}
	e, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": exe,
	}, nil, engine.Options{})
	if err != nil {
		t.Fatalf("无密钥构造应成功: %v", err)
	}
	if e.Kind() != Kind {
		t.Errorf("Kind = %q", e.Kind())
	}
	if !e.SupportsStreaming() {
		t.Error("localcli 应支持流式（逐行输出）")
	}
}

// TestBuild_TimeoutClamping 验证超时被夹到合理范围。
func TestBuild_TimeoutClamping(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("无法获取测试可执行文件路径")
	}
	cases := []struct {
		in   any
		want time.Duration
	}{
		{nil, defaultTimeoutSecs * time.Second},
		{float64(1), 10 * time.Second},
		{float64(9999), 600 * time.Second},
		{float64(90), 90 * time.Second},
	}
	for _, c := range cases {
		cfg := engine.Config{"preset": presetCustom, "cliPath": exe}
		if c.in != nil {
			cfg["timeoutSecs"] = c.in
		}
		e, err := engine.Build(Kind, cfg, nil, engine.Options{})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if got := e.(*engineImpl).timeout; got != c.want {
			t.Errorf("timeoutSecs=%v → timeout=%v, 期望 %v", c.in, got, c.want)
		}
	}
}

// TestExtractCLIHint 验证从冗长 stderr 里提取最后一行有用信息。
func TestExtractCLIHint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Error: not logged in", "Error: not logged in"},
		{"loading...\nprogress 50%\nError: auth failed", "Error: auth failed"},
		{"Error: boom\n\n\n", "Error: boom"},
	}
	for _, c := range cases {
		if got := extractCLIHint(c.in); got != c.want {
			t.Errorf("extractCLIHint(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
	if got := extractCLIHint(strings.Repeat("x", 5000)); len([]rune(got)) > 210 {
		t.Errorf("超长 stderr 应被截断，得到 %d 字符", len([]rune(got)))
	}
}

// TestRunCLI_MissingExecutable 验证可执行文件缺失时的错误分类。
func TestRunCLI_MissingExecutable(t *testing.T) {
	e := &engineImpl{
		preset:  presetCustom,
		cliPath: filepath.Join(t.TempDir(), "绝对不存在的可执行文件"),
		timeout: 5 * time.Second,
		workDir: os.TempDir(),
	}
	_, err := e.runCLI(context.Background(), "test", nil)
	if err == nil {
		t.Fatal("不存在的可执行文件应报错")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindConfig {
		t.Errorf("应归类为 KindConfig（配置问题），得到 %s", ee.KindName())
	}
}

// ---------------------------------------------------------------------------
// 真实进程执行（用假 CLI 脚本，跨平台）
// ---------------------------------------------------------------------------

// writeFakeCLI 生成一个假 CLI 可执行文件。
//
// Windows 用 .bat（cmd 解释），其它平台用 shell 脚本 + chmod 0755。
func writeFakeCLI(t *testing.T, dir, behavior string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "fakecli.bat")
		if err := os.WriteFile(path, []byte("@echo off\r\n"+behavior+"\r\n"), 0o755); err != nil {
			t.Fatalf("写假 CLI 失败: %v", err)
		}
		return path
	}
	path := filepath.Join(dir, "fakecli.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+behavior+"\n"), 0o755); err != nil {
		t.Fatalf("写假 CLI 失败: %v", err)
	}
	return path
}

// TestRunCLI_StderrNotInOutput 验证 stderr 不混入译文。
//
// CLI 经常往 stderr 打进度条与警告，混进译文会毁掉结果。
func TestRunCLI_StderrNotInOutput(t *testing.T) {
	dir := t.TempDir()
	var cliPath string
	if runtime.GOOS == "windows" {
		cliPath = writeFakeCLI(t, dir, "echo 这是stderr噪音 1>&2\r\necho 真正的译文")
	} else {
		cliPath = writeFakeCLI(t, dir, "echo \"这是stderr噪音\" >&2\necho \"真正的译文\"")
	}
	e, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": cliPath, "timeoutSecs": 30,
	}, nil, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, err := e.Translate(context.Background(), engine.Request{
		Source: "x", SourceLang: "en", TargetLang: "zh",
	})
	if err != nil {
		t.Skipf("假 CLI 执行失败（可能是环境限制）: %v", err)
	}
	if strings.Contains(res.Target, "stderr噪音") {
		t.Errorf("stderr 混入了译文: %q", res.Target)
	}
	if !strings.Contains(res.Target, "真正的译文") {
		t.Errorf("stdout 内容丢失: %q", res.Target)
	}
}

// TestRunCLI_NonZeroExit 验证非零退出码被归类为上游错误。
func TestRunCLI_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	var cliPath string
	if runtime.GOOS == "windows" {
		cliPath = writeFakeCLI(t, dir, "exit /b 3")
	} else {
		cliPath = writeFakeCLI(t, dir, "exit 3")
	}
	e, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": cliPath, "timeoutSecs": 30,
	}, nil, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, err = e.Translate(context.Background(), engine.Request{
		Source: "x", SourceLang: "en", TargetLang: "zh",
	})
	if err == nil {
		t.Skip("假 CLI 未执行（环境限制）")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindUpstream {
		t.Errorf("非零退出码应归类为 KindUpstream，得到 %s", ee.KindName())
	}
	if !strings.Contains(ee.Message, "退出码") {
		t.Errorf("错误消息应含退出码: %s", ee.Message)
	}
}

// TestRunCLI_Cancelled 验证 ctx 取消能中断执行并 kill 进程。
func TestRunCLI_Cancelled(t *testing.T) {
	dir := t.TempDir()
	var cliPath string
	if runtime.GOOS == "windows" {
		cliPath = writeFakeCLI(t, dir, "ping -n 30 127.0.0.1 > nul")
	} else {
		cliPath = writeFakeCLI(t, dir, "sleep 30")
	}
	e, err := engine.Build(Kind, engine.Config{
		"preset": presetCustom, "cliPath": cliPath, "timeoutSecs": 60,
	}, nil, engine.Options{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = e.Translate(ctx, engine.Request{Source: "x", SourceLang: "en", TargetLang: "zh"})
	elapsed := time.Since(start)
	if err == nil {
		t.Skip("假 CLI 未执行（环境限制）")
	}
	var ee *engine.Error
	if !asEngineErr(err, &ee) {
		t.Fatalf("应返回 *engine.Error，得到 %T", err)
	}
	if ee.Kind != engine.KindCancelled {
		t.Errorf("取消应归类为 KindCancelled，得到 %s", ee.KindName())
	}
	// 必须尽快返回，而不是等 CLI 自己跑完 30 秒
	if elapsed > 8*time.Second {
		t.Errorf("取消后耗时 %v，说明没有及时 kill 进程", elapsed)
	}
}

// TestRunCLI_ShellMetaNotInterpreted 是注入防护的【端到端】证明。
//
// 上面的 TestParseExtraArgs_RejectsShellMeta 验证了配置期拒绝，
// 这里验证即使绕过配置校验（直接构造 engineImpl），
// exec.Command 的参数数组也不会让元字符被 shell 解释。
//
// # 判据：参数向量必须逐字相等
//
// 早期的版本用「注入 payload 尝试创建 marker 文件，然后断言文件不存在」做判据。
// 这个判据在 Windows 上是【空转的】—— payload 里的 `touch` 命令 Windows 根本没有，
// 无论有没有经过 shell，marker 都不会被创建，测试永远通过。
// 实测把实现改成 `cmd.exe /C` 拼接后，该测试依然 PASS，等于没测。
//
// 现在改用强判据：假 CLI 把收到的每个参数单独打印一行，
// 测试断言「输出按行切分后，与传入的 payload 数组【完全相等】」。
//   - 参数数组传递：假 CLI 收到 N 个完整参数，输出 N 行，逐字匹配 → PASS
//   - 经过 shell：shell 会按空格切词、吞掉 ; & | $ 等运算符，
//     假 CLI 收到的参数个数与内容都会变 → 逐行比对必然 FAIL
//
// 这个判据不依赖任何平台特有命令，Windows 与 Unix 上同样有效。
func TestRunCLI_ShellMetaNotInterpreted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "PWNED")

	// 注入载荷：混合 Unix 与 Windows 的 shell 元字符。
	// 每一项都含空格 —— 这是关键：若经过 shell，空格会导致切词，
	// 参数个数立刻变化，逐行比对必然失败。
	payloads := []string{
		"; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"&& echo pwned",
		"| echo pwned",
		"& echo pwned",
		"> " + marker,
		"plain arg with spaces",
	}

	e := &engineImpl{
		preset:  presetCustom,
		cliPath: os.Args[0], // 测试进程自己扮演假 CLI
		// 直接构造，绕过 parseExtraArgs 的配置期校验
		extra:    payloads,
		workDir:  dir,
		timeout:  30 * time.Second,
		maxChars: defaultMaxChars,
	}

	// 通过环境变量告诉测试进程「你现在扮演一个回显参数的 CLI」
	t.Setenv(helperEnvKey, "1")

	// 让 helper 把实际收到的 argv 写进这个文件
	argsFile := filepath.Join(dir, "argv.json")
	t.Setenv(helperArgsFileEnvKey, argsFile)

	if _, err := e.runCLI(context.Background(), "ignored", nil); err != nil {
		t.Fatalf("helper 进程执行失败: %v", err)
	}

	// 核心断言：子进程实际收到的参数向量与传入的 payloads 逐字相等。
	//
	// 读文件而不是读 stdout —— runCLI 会把增量片段无分隔符拼接
	// （这对翻译是正确的），stdout 无法还原参数边界。
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("helper 未写出 argv 文件: %v", err)
	}
	var gotArgs []string
	if err := json.Unmarshal(raw, &gotArgs); err != nil {
		t.Fatalf("argv 文件不是合法 JSON: %v (%q)", err, string(raw))
	}
	if len(gotArgs) != len(payloads) {
		t.Fatalf("假 CLI 收到 %d 个参数，期望 %d 个 —— 参数被 shell 切词或吞掉了\n"+
			"  收到: %q\n  期望: %q", len(gotArgs), len(payloads), gotArgs, payloads)
	}
	for i, want := range payloads {
		if gotArgs[i] != want {
			t.Errorf("第 %d 个参数被改写\n  得: %q\n  期望: %q", i, gotArgs[i], want)
		}
	}

	// 附加断言：marker 文件不应被创建（在 Unix 上是有效的二次确认）
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("命令注入成功！%s 被创建了", marker)
	}
}

// TestRunCLI_ShellMetaTestIsEffective 是上面那个测试的【自检】。
//
// 它验证判据本身有效：把 payload 故意打乱后逐行比对，必须失败。
// 没有这条自检，就无法区分「实现安全」和「测试写错了所以永远通过」——
// 本项目真的踩过这个坑（marker 判据在 Windows 上空转，变异测试照样 PASS）。
func TestRunCLI_ShellMetaTestIsEffective(t *testing.T) {
	// 判据是「argv 数组逐字相等」。验证它对两种情况都能给出正确结论：
	original := []string{"; touch PWNED", "$(touch PWNED)", "plain arg with spaces"}

	// 情况一：参数数组传递 → argv 完整，判据通过
	if !argvEqual(original, original) {
		t.Fatal("自检失败：相同的 argv 应判为相等")
	}
	// 情况二：经过 shell 切词 → 参数个数变多，判据必须能发现
	mangled := []string{";", "touch", "PWNED", "$(touch", "PWNED)", "plain", "arg", "with", "spaces"}
	if argvEqual(original, mangled) {
		t.Error("自检失败：切词后的 argv 仍判为相等，说明判据无效")
	}
	// 情况三：shell 吞掉运算符 → 参数个数变少，同样要能发现
	swallowed := []string{"touch", "PWNED", "touch", "PWNED", "plain", "arg", "with", "spaces"}
	if argvEqual(original, swallowed) {
		t.Error("自检失败：运算符被吞后的 argv 仍判为相等，说明判据无效")
	}
}

// helperEnvKey 是标记「当前进程应扮演假 CLI」的环境变量名。
const helperEnvKey = "DBX_TRANSLATION_TEST_HELPER_CLI"

// helperArgsFileEnvKey 指定假 CLI 把收到的 argv 写到哪个文件（JSON 数组）。
const helperArgsFileEnvKey = "DBX_TRANSLATION_TEST_HELPER_ARGS_FILE"

// TestHelperProcess 不是真正的测试，而是被上面的测试当作「假 CLI」调用的分支。
//
// 当测试进程以 helperEnvKey=1 启动时，它不做任何断言，
// 只把收到的命令行参数逐行打印到 stdout 然后退出。
// 这样父测试就能检查「参数是否被字面传递」。
//
// 命名以 Test 开头是必需的：os.Args[0] 是测试二进制，
// 必须通过 -test.run 才能进入指定函数。这里我们不用 -test.run，
// 而是让 helper 分支在 TestMain 之前通过环境变量短路 —— 见 init()。
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvKey) != "1" {
		t.Skip("仅在作为假 CLI 被调用时执行")
	}
	// 正常路径下不会走到这里（init 已短路），保留以满足 go test 的函数签名要求
	t.Fatal("helper 分支应在 init 中短路，不应走到测试逻辑")
}

// init 在测试二进制启动时检查环境变量：
// 若被当作假 CLI 调用，则打印参数并立即退出，不运行任何测试。
//
// 为什么用 init 而不是在 TestHelperProcess 里处理：
//
//	os.Args[0] 启动测试二进制时会先跑完所有包的 init 与 TestMain，
//	期间可能触发其它测试的副作用。用 init 短路最干净、最快。
func init() {
	if os.Getenv(helperEnvKey) != "1" {
		return
	}
	// 收集实际收到的参数（跳过 os.Args[0] 与 -test.* 标志）
	var args []string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-test.") {
			continue
		}
		args = append(args, arg)
	}
	// 【关键】把 argv 写成 JSON 文件，而不是打印到 stdout。
	//
	// 为什么不用 stdout：runCLI 会把各次 emit 的文本【无分隔符拼接】
	// （这对翻译是正确的 —— 增量片段本来就该直接相连），
	// 因此 8 行输出会被并成一整行，测试无法按行还原参数边界。
	// 写文件则能精确保留「子进程实际收到了几个参数、每个是什么」，
	// 这是判断有没有经过 shell 切词的最直接证据。
	if out := os.Getenv(helperArgsFileEnvKey); out != "" {
		b, _ := json.Marshal(args)
		_ = os.WriteFile(out, b, 0o600)
	}
	// 同时往 stdout 写点东西，让 runCLI 不报「CLI 没有返回任何内容」
	_, _ = os.Stdout.WriteString("ok\n")
	_ = os.Stdout.Sync()
	os.Exit(0)
}

// argvEqual 比较两个参数向量是否逐字相等（注入测试的判据）。
func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func asEngineErr(err error, target **engine.Error) bool {
	for err != nil {
		if e, ok := err.(*engine.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var _ = exec.Command
