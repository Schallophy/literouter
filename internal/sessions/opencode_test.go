package sessions

import (
	"testing"
	"time"
)

// lockTime pins "now" for the status windows so the tests do not depend on when
// they run. It returns a function that restores the clock.
func lockTime(unixMillis int64) func() {
	original := timeNow
	timeNow = func() time.Time { return time.UnixMilli(unixMillis) }
	return func() { timeNow = original }
}

func assistantJSON(finish, errType, errMessage string, completed int64, created int64) string {
	errorPart := ""
	if errType != "" || errMessage != "" {
		errorPart = `,"error":{"type":"` + errType + `","message":"` + errMessage + `"}`
	}
	finishPart := ""
	if finish != "" {
		finishPart = `,"finish":"` + finish + `"`
	}
	return `{"content":[],"time":{"created":` + itoa64(created) + `,"completed":` + itoa64(completed) + `}` +
		finishPart + errorPart + `}`
}

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 20)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

const nowMillis = int64(1795320000000)

func TestOpencodeStatusFailedWithIdle(t *testing.T) {
	defer lockTime(nowMillis)()
	idle := `{"outcome":"failed","time":{"created":1795319999000}}`
	assistant := assistantJSON("error", "provider.free-limit", "Free usage limit reached.", 1795319999000, 1795319990000)
	status := opencodeStatus(idle, 10, assistant, 9, 8, 1795319999500)
	if !status.Stuck() || status.Code != "free-limit" {
		t.Fatalf("status = %+v, want stuck/free-limit", status)
	}
	if status.Detail == "" {
		t.Fatalf("provider detail dropped")
	}
}

func TestOpencodeStatusIdleOvertakenByNewUserMessage(t *testing.T) {
	defer lockTime(nowMillis)()
	// 用户在失败之后又发了一条消息，正在跑：不能再报「卡住」。
	idle := `{"outcome":"failed","time":{"created":1795319000000}}`
	assistant := assistantJSON("stop", "", "", 1795319000000, 1795318990000)
	status := opencodeStatus(idle, 10, assistant, 9, 11, nowMillis)
	if status.State != "active" {
		t.Fatalf("status = %+v, want active", status)
	}
}

func TestOpencodeStatusUserMessageAbandoned(t *testing.T) {
	defer lockTime(nowMillis)()
	// 用户发了消息、回复从未落地、且已经过了时间窗：这轮真的掉了。
	idle := `{"outcome":"succeeded","time":{"created":1795310000000}}`
	assistant := assistantJSON("stop", "", "", 1795310000000, 1795310000000)
	status := opencodeStatus(idle, 10, assistant, 10, 11, nowMillis-20*60*1000)
	if !status.Stuck() || status.Code != "interrupted" {
		t.Fatalf("status = %+v, want stuck/interrupted", status)
	}
}

func TestOpencodeStatusLegacyStoreWithoutIdle(t *testing.T) {
	deferred := lockTime(nowMillis)
	defer deferred()
	// 旧库没有 idle 行，只有 assistant；被掐断的那一轮没有 finish。
	assistant := assistantJSON("", "", "", 0, nowMillis-3*60*1000)
	status := opencodeStatus("", -1, assistant, 5, 5, nowMillis)
	if status.State != "active" {
		t.Fatalf("status = %+v, want active (within window)", status)
	}

	assistant = assistantJSON("", "", "", 0, nowMillis-15*60*1000)
	status = opencodeStatus("", -1, assistant, 5, 5, nowMillis)
	if !status.Stuck() || status.Code != "interrupted" {
		t.Fatalf("status = %+v, want stuck/interrupted after the window", status)
	}
}

func TestOpencodeStatusErrorWithoutIdle(t *testing.T) {
	defer lockTime(nowMillis)()
	// 崩溃的构建只留下 error 载荷，没有 idle 行。
	assistant := assistantJSON("error", "provider.upstream", "[code=upstream_error]", 1795320000000, 1795319990000)
	status := opencodeStatus("", -1, assistant, 5, 4, nowMillis)
	if !status.Stuck() || status.Code != "upstream" {
		t.Fatalf("status = %+v, want stuck/upstream", status)
	}
}

func TestOpencodeStatusLongToolStillActive(t *testing.T) {
	defer lockTime(nowMillis)()
	assistant := assistantJSON("tool-calls", "", "", 0, nowMillis-10*60*1000)
	status := opencodeStatus("", -1, assistant, 5, 4, nowMillis)
	if status.State != "active" {
		t.Fatalf("status = %+v, want active inside the tool window", status)
	}

	assistant = assistantJSON("tool-calls", "", "", 0, nowMillis-45*60*1000)
	status = opencodeStatus("", -1, assistant, 5, 4, nowMillis)
	if !status.Stuck() {
		t.Fatalf("status = %+v, want stuck after the tool window", status)
	}
}

func TestOpencodeStatusSucceeded(t *testing.T) {
	defer lockTime(nowMillis)()
	idle := `{"outcome":"succeeded","time":{"created":1795319999000}}`
	assistant := assistantJSON("stop", "", "", 1795319999000, 1795319990000)
	status := opencodeStatus(idle, 10, assistant, 10, 9, nowMillis)
	if status.State != "done" {
		t.Fatalf("status = %+v, want done", status)
	}
}
