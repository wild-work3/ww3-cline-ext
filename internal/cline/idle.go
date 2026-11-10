package cline

import (
	"errors"
	"io"
	"sync"
	"time"
)

// ErrIdleTimeout 表示上游流长时间无数据。
//
// 为什么必需：本渠道 StreamHTTP **没有总超时**（大 prompt 的首字节可能远超 120s，
// 设总超时会把正常的长思考请求砍掉），因此必须靠「无数据即断」的看门狗兜底，
// 否则上游半开连接会让 worker 永久挂住（占满 MaxWorkers）。
var ErrIdleTimeout = errors.New("idle timeout: 上游流空闲超时")

// NewIdleReader 包装 io.ReadCloser，超过 idle 无数据即关闭底层并返回 ErrIdleTimeout。
func NewIdleReader(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if rc == nil {
		return nil
	}
	if idle <= 0 {
		return rc
	}
	r := &idleReader{rc: rc, idle: idle}
	r.timer = time.AfterFunc(idle, r.fire)
	return r
}

type idleReader struct {
	rc   io.ReadCloser
	idle time.Duration

	mu    sync.Mutex
	timer *time.Timer
	fired bool
}

func (r *idleReader) fire() {
	r.mu.Lock()
	r.fired = true
	r.mu.Unlock()
	_ = r.rc.Close()
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.mu.Lock()
	if r.fired {
		r.mu.Unlock()
		return n, ErrIdleTimeout
	}
	if n > 0 && r.timer != nil {
		r.timer.Reset(r.idle)
	}
	r.mu.Unlock()
	return n, err
}

func (r *idleReader) Close() error {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.rc.Close()
}
