package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestElapsedMs_RoundsUp 验证向上取整：可测量的非零耗时至少报 1ms。
//
// 为什么需要向上取整：time.Duration.Milliseconds() 是【向下截断】，
// 0.4ms 会显示成 0ms。界面上「耗时 0ms」看着像计时功能坏了，
// 而实际上确实花了时间、只是不足 1ms。向上取整更诚实。
//
// ⚠️ 注意它【不能】解决「time.Since 本身就等于 0」的情况（0 取整还是 0），
// 那属于时钟粒度问题，见 TestElapsedMs_ClockGranularityIsReal。
func TestElapsedMs_RoundsUp(t *testing.T) {
	// 用「当前时刻减去固定回拨量」构造 start，
	// 不依赖真实 sleep —— 那样测试会慢，且时长本身有抖动，反而引入新的不稳定。
	cases := []struct {
		name string
		ago  time.Duration
		want int64
	}{
		{"1ns 应进位到 1ms", time.Nanosecond, 1},
		{"1µs 应进位到 1ms", time.Microsecond, 1},
		{"不足 1ms（999µs）应进位到 1ms", 999 * time.Microsecond, 1},
		{"恰好 1ms", time.Millisecond, 1},
		{"1ms 多 1ns 应进位到 2ms", time.Millisecond + time.Nanosecond, 2},
		{"恰好 5ms", 5 * time.Millisecond, 5},
		{"5ms 多 1µs 应进位到 6ms", 5*time.Millisecond + time.Microsecond, 6},
		{"1 秒", time.Second, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now().Add(-c.ago)
			got := ElapsedMs(start)
			// 允许真实执行期间又流逝极短时间，使结果比理论值大 1ms；
			// 但绝不能小于理论值（那意味着没向上取整）。
			if got < c.want {
				t.Errorf("ElapsedMs = %d, 期望 >= %d（向上取整失败）", got, c.want)
			}
			if got > c.want+1 {
				t.Errorf("ElapsedMs = %d, 远超期望 %d（取整逻辑可能有误）", got, c.want)
			}
		})
	}
}

// TestElapsedMs_NeverNegative 验证任何输入都不得返回负数。
//
// 这是 ElapsedMs 唯一需要保证的硬性质：时钟回拨（系统时间被向前调整过）时
// time.Since 会得到负的 Duration，若不处理，界面会显示「-3ms」这种荒谬值。
func TestElapsedMs_NeverNegative(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
	}{
		{"start 在 1 小时后（时钟回拨）", time.Now().Add(time.Hour)},
		{"start 在 1 年后", time.Now().Add(365 * 24 * time.Hour)},
		{"start 就是现在", time.Now()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ElapsedMs(c.start); got < 0 {
				t.Errorf("ElapsedMs = %d, 不得为负数", got)
			}
		})
	}
}

// TestElapsedMs_ZeroIsLegal 验证「耗时 0」是合法返回值，不是缺陷。
//
// 这条测试固化的是 v0.2.2 CI flaky test 的【真正教训】：
// 原先 openai_test.go 断言 `ElapsedMs <= 0` 即报错，等于假设「一次翻译至少花 1ms」。
// 该假设在快机器上不成立，于是同一份代码在 CI 上随机红绿。
//
// 实测数据（Windows 本机，200 次进程内 httptest 往返）：
//   - time.Since(start) == 0 的有 177 次（88.5%）
//   - 连续两次 time.Now() 差值为 0 的有 200 次（100%）
//
// 即 Go 在这台机器上的时钟粒度粗到「连续读两次得到同一个值」，
// 此时 time.Since 真的是 0，向上取整也救不回来（0 取整还是 0）。
//
// 结论：断言必须来自规范，不能来自「实现现在输出什么」（对齐 AGENTS.md 6.8）。
// 机器速度不是规范的一部分，把它当规范就必然 flaky。
func TestElapsedMs_ZeroIsLegal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := srv.Client()
	zeroCount := 0
	for i := 0; i < 50; i++ {
		start := time.Now()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()

		got := ElapsedMs(start)
		// 唯一硬要求：非负。0 完全合法（快机器 + 粗时钟）。
		if got < 0 {
			t.Fatalf("第 %d 次：ElapsedMs = %d, 不得为负数", i, got)
		}
		if got == 0 {
			zeroCount++
		}
	}
	// 把实测现象记录下来，便于日后读日志的人理解「为什么这里允许 0」
	t.Logf("50 次进程内 HTTP 往返中，ElapsedMs==0 的有 %d 次（0 是合法值）", zeroCount)
}
