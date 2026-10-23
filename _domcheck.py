# -*- coding: utf-8 -*-
"""前端静态闸门：DOM 悬空引用 / 关键元素 / 重复函数声明。

与 _buildpkg.js 内建的自检同规则，方便单独快速跑（不需要先编译侧车）。

规则来源（都是 dbx-todo 记录过的真实事故）：

  1) app.js 里出现 `$("x").` —— 对 getElementById 结果直接取属性。
     x 不存在时是 `Cannot set properties of null`，而它通常位于绑定/boot
     开头，会把整个界面连坐搞死（历史事故：所有按钮失效 + 数据从不落盘）。
     本项目已改用 val(id)/setVal(id,v) 与 click()/on() 安全绑定，但仍要守住。

  2) `click("x", fn)` / `on("x", ev, fn)` 是安全绑定；缺失元素只告警不致命。

  3) index.html 里被注释掉的 id 会被单独列出（最容易被漏掉的一类）。

  4) 同名函数声明会被后声明的覆盖，先声明的那个静默失效 —— 必须静态兜住。
     历史事故：confirmModal 定义两次 → 删除按钮里传字符串进去
     → `lines.join is not a function` 抛错。

  5) 关键元素清单（CRITICAL）必须都在 index.html 真实存在。

  6) 功能开关必须与 HTML 一致：开关 true 就得有元素，false 就不能有。
"""
import io
import os
import re
import sys

BASE = os.path.dirname(os.path.abspath(__file__))
UI = os.path.join(BASE, "ui")

def read(name):
    with io.open(os.path.join(UI, name), encoding="utf-8") as f:
        return f.read()

html = read("index.html")
app = read("app.js")
storage = read("storage.js")
engines = read("engines.js")
history = read("history.js")

html_live = re.sub(r"<!--.*?-->", "", html, flags=re.S)
live_ids = set(re.findall(r'\bid="([^"]+)"', html_live))
commented_ids = sorted(set(re.findall(r'\bid="([^"]+)"', html)) - live_ids)

fails = []
warns = []

print("HTML 可用 id (%d)" % len(live_ids))
if commented_ids:
    print("被注释掉的 id: %s" % ", ".join(commented_ids))
print()

# ---------- ① 悬空属性访问 ----------
# 扫描前去掉注释：注释里会引用历史错误代码，不能当真
def strip_js(src):
    src = re.sub(r"/\*.*?\*/", "", src, flags=re.S)
    src = re.sub(r"^[ \t]*//.*$", "", src, flags=re.M)
    return src

for fname, src in (("app.js", app), ("engines.js", engines),
                   ("history.js", history), ("storage.js", storage)):
    code = strip_js(src)
    for m in re.finditer(r'\$\("([^"]+)"\)\s*\.', code):
        rid = m.group(1)
        if rid not in live_ids:
            line = code[:m.start()].count("\n") + 1
            fails.append('%s 第 %d 行：对不存在的 #%s 直接取属性（会抛 TypeError 并连坐启动）'
                         % (fname, line, rid))

if fails:
    print("!! 不安全引用（必须修）:")
    for f in fails:
        print("   - " + f)
else:
    print("OK：没有对缺失元素取属性的代码。")

# ---------- ② 同名函数声明覆盖 ----------
dupes_all = []
for fname, src in (("app.js", app), ("engines.js", engines),
                   ("history.js", history), ("storage.js", storage)):
    code = strip_js(src)
    sigs = {}
    # 匹配 2 空格缩进的顶层函数声明（IIFE 内的模块级函数）
    for m in re.finditer(r'^  function (\w+)\s*\(', code, flags=re.M):
        sigs.setdefault(m.group(1), []).append(code[:m.start()].count("\n") + 1)
    for name, lines in sigs.items():
        if len(lines) > 1:
            dupes_all.append("%s: %s 定义于第 %s 行" % (fname, name, ", ".join(str(x) for x in lines)))

if dupes_all:
    print("!! 重复的函数声明（后者会覆盖前者，必须改名）:")
    for d in dupes_all:
        print("   - " + d)
else:
    print("OK：没有重复的函数声明。")

# ---------- ③ 关键元素清单 ----------
CRITICAL = [
    # 骨架
    "app", "topbar", "main", "statusbar", "toast",
    # 视图切换
    "btn-view-translate", "btn-view-engines", "btn-view-history", "btn-view-settings",
    "view-translate", "view-engines", "view-history", "view-settings",
    # 翻译页
    "sel-source-lang", "sel-target-lang", "sel-engine", "btn-swap",
    "src-text", "src-count", "dst-text", "dst-wrap", "dst-count", "dst-empty",
    "dst-cursor", "btn-translate", "btn-cancel", "btn-copy-dst", "btn-clear-src",
    "btn-clear-dst", "btn-paste", "chk-instant", "chk-stream",
    "translate-hint", "translate-meta", "no-engine", "btn-add-first-engine",
    # 引擎页
    "engine-list", "engine-empty", "btn-add-engine",
    # 引擎弹框
    "engine-modal", "engine-modal-title", "engine-kind", "engine-kind-desc",
    "engine-fields", "engine-form-error", "btn-engine-save", "btn-engine-test",
    "btn-engine-cancel", "btn-engine-delete", "btn-close-engine-modal",
    # 历史页
    "history-list", "history-empty", "history-search", "history-chips",
    "history-count", "history-pager", "btn-history-prev", "btn-history-next",
    "btn-clear-history",
    # 设置页
    "set-instant-enabled", "set-instant-debounce", "set-instant-minchars",
    "set-instant-maxlength", "set-stream-enabled", "set-chunk-delay",
    "set-history-limit", "set-default-source", "set-default-target",
    "btn-save-settings", "settings-status",
    # 外观只读展示（跟随宿主，无手动覆盖控件）
    "info-theme", "info-locale", "info-font", "info-token-count",
    "info-datadir", "info-dbpath", "info-version", "info-secret",
    "btn-reconnect", "btn-settings-export", "btn-settings-import", "btn-settings-diag",
    # 警示条
    "banner-not-persistent", "banner-not-persistent-text", "btn-banner-retry",
    "banner-insecure-secret", "banner-insecure-secret-text",
    # 弹框与菜单
    "more-menu", "btn-more", "btn-export", "btn-import", "btn-diag", "btn-swap-lang",
    "confirm-modal", "confirm-title", "confirm-lines", "btn-confirm-yes", "btn-confirm-no",
    "diag-modal", "diag-content", "btn-diag-copy", "btn-close-diag",
    "file-input", "engine-badge", "status-dot", "status-text", "status-version",
]
missing = [c for c in CRITICAL if c not in live_ids]
if missing:
    print("!! index.html 缺关键元素 (%d): %s" % (len(missing), ", ".join(missing)))
else:
    print("OK：关键元素齐备（%d 项）。" % len(CRITICAL))

# ---------- ④ 功能开关与 HTML 一致 ----------
app_code = strip_js(app)
m = re.search(r'var\s+SHOW_DIAG_BAR\s*=\s*(true|false)', app_code)
if not m:
    print("!! app.js 缺少 SHOW_DIAG_BAR 开关（置顶诊断条的状态无法自证）")
elif m.group(1) == "true" and "diag-bar" not in live_ids:
    print("!! SHOW_DIAG_BAR=true 但 index.html 里 #diag-bar 不存在")
elif m.group(1) == "false" and "diag-bar" in live_ids:
    print("!! SHOW_DIAG_BAR=false 但 index.html 里 #diag-bar 仍存在（上线不该出现）")
else:
    print("OK：SHOW_DIAG_BAR=%s 与 HTML 一致。" % m.group(1))

m2 = re.search(r'var\s+ENABLE_IMPORT\s*=\s*(true|false)', app_code)
if not m2:
    print("!! app.js 缺少 ENABLE_IMPORT 开关")
else:
    has_btn = "btn-import" in live_ids and "file-input" in live_ids
    if m2.group(1) == "true" and not has_btn:
        print("!! ENABLE_IMPORT=true 但 #btn-import / #file-input 缺失")
    elif m2.group(1) == "false" and has_btn:
        print("!! ENABLE_IMPORT=false 但导入入口仍在 HTML 里")
    else:
        print("OK：ENABLE_IMPORT=%s 与 HTML 一致。" % m2.group(1))

# ---------- ⑤ 安全约束 ----------
# 译文与用户输入绝不能用 innerHTML（上游返回内容是不可信数据）
for fname, src in (("app.js", app), ("engines.js", engines),
                   ("history.js", history), ("storage.js", storage)):
    code = strip_js(src)
    for m in re.finditer(r'\.innerHTML\s*=', code):
        line = code[:m.start()].count("\n") + 1
        print("!! %s 第 %d 行使用了 innerHTML 赋值（XSS 风险，改用 textContent）" % (fname, line))

# localStorage 在 opaque origin 沙箱里会抛 SecurityError
for fname, src in (("app.js", app), ("engines.js", engines),
                   ("history.js", history), ("storage.js", storage)):
    code = strip_js(src)
    if re.search(r'\blocalStorage\b|\bsessionStorage\b', code):
        print("!! %s 引用了 localStorage/sessionStorage（沙箱里会抛 SecurityError）" % fname)

# 桥接用法：API 永远是 window.dbxPlugin 本身，不能把 ready 的结果当 API
if re.search(r'dbxPlugin\.ready\.then\(\s*boot\s*\)', app_code):
    print("!! app.js 用 dbxPlugin.ready 卡住启动（宿主不发 init 时会永远空白）")
if "window.dbxPlugin" not in storage:
    print("!! storage.js 未以 window.dbxPlugin 为 API 对象")
if 'invoke(method' not in storage and "api.invoke(" not in storage:
    print("!! storage.js 未使用 invoke(method, params, {timeoutMs}) 形态")

print("OK：安全约束检查完成。")

# ---------- 汇总 ----------
print()
if fails or dupes_all or missing:
    print("RESULT: FAIL")
    sys.exit(1)
print("RESULT: PASS")
