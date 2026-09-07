package credentials

import "time"

// timeNowUnix 可注入的时钟，便于测试避免依赖真实时间。
var timeNowUnix = func() int64 { return time.Now().Unix() }

// SetClock 替换时钟（测试用）。
func SetClock(fn func() int64) { timeNowUnix = fn }
