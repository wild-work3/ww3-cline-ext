package cline

import (
	"io"
	"strings"
	"testing"
	"time"

	"wild-work3/cline-ext/internal/protocol/errkind"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errkind.Kind
	}{
		{500, `{"error":{"message":"upstream error from Openrouter: request failed with status 429 Too Many Requests"}}`, errkind.SoftRate},
		{500, `{"error":{"message":"boom"}}`, errkind.Server},
		{500, `{"error":{"message":"Daily limit reached for cline-free/x"}}`, errkind.Passthrough},
		{403, `{"error":{"message":"API_REQUEST_ERROR_CODE"}}`, errkind.BadParams},
		{403, `{"error":{"message":"forbidden"}}`, errkind.Client},
		{401, `{"error":{"message":"unauthorized"}}`, errkind.SessionDead},
		{402, `{"error":{"message":"payment required"}}`, errkind.HardCredit},
		{500, `{"error":{"message":"empty response content"}}`, errkind.BadParams},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%s want %s", c.status, c.body, got, c.want)
		}
	}
}

func TestInnerStatus(t *testing.T) {
	if got := InnerStatus("... failed with status 429 Too Many"); got != 429 {
		t.Fatalf("InnerStatus=%d", got)
	}
	if got := InnerStatus("no code"); got != 0 {
		t.Fatalf("InnerStatus=%d", got)
	}
}

func TestParseStreamErrFrame(t *testing.T) {
	body := "data: {\"id\":\"gen_01\",\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"Failed to create stream: request failed with status 429\"}}\n\ndata: [DONE]\n\n"
	_, _, err := ParseStream(strings.NewReader(body), func(string, any) error { return nil }, "m")
	e := AsError(err, "")
	if err == nil || e.Kind != errkind.Passthrough {
		t.Fatalf("err=%v kind=%v", err, e.Kind)
	}
}

func TestParseStreamOK(t *testing.T) {
	body := "data: {\"id\":\"gen-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"
	var txt string
	u, fin, err := ParseStream(strings.NewReader(body), func(ev string, p any) error {
		if ev == "text.delta" {
			txt += p.(map[string]any)["content"].(string)
		}
		return nil
	}, "m")
	if err != nil || txt != "hi" || fin != "stop" || u == nil || u.TotalTokens != 5 || u.Cache == nil || u.Cache.Cached != 1 {
		t.Fatalf("err=%v txt=%q fin=%q u=%+v", err, txt, fin, u)
	}
}

func TestIdleReader(t *testing.T) {
	r, w := io.Pipe()
	ir := NewIdleReader(r, 120*time.Millisecond)
	defer ir.Close()
	go func() { time.Sleep(400 * time.Millisecond); w.Write([]byte("late")); w.Close() }()
	buf := make([]byte, 8)
	_, err := ir.Read(buf)
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("err=%v", err)
	}
}

func TestClampMaxTokens(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"max_tokens":8}`, `{"max_tokens":64}`},
		{`{"max_tokens":1000000}`, `{"max_tokens":200000}`},
		{`{"max_tokens":0}`, `{}`},
		{`{"max_tokens":-1}`, `{}`},
		{`{"max_tokens":500}`, `{"max_tokens":500}`},
		{`{}`, `{}`},
	} {
		got := string(clampMaxTokens([]byte(c.in)))
		if got != c.want {
			t.Errorf("clampMaxTokens(%s)=%s want %s", c.in, got, c.want)
		}
	}
}

func TestRouteOf(t *testing.T) {
	if RouteOf("gen-1759999999-AbCdEf") != "openrouter" || RouteOf("gen_01HXYZ") != "cline_gateway" || RouteOf("x") != "unknown" {
		t.Fatal("route")
	}
}
