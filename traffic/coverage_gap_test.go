package traffic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autumn-27/artex/traffic/rewrite"
)

func TestSplitLegacyEvidence(t *testing.T) {
	t.Run("normal_headers_and_body", func(t *testing.T) {
		input := "Host: example.com\r\nContent-Type: text/html\r\n\r\nbody content"
		head, body, err := splitLegacyEvidence(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(head, "Host: example.com") {
			t.Fatalf("head = %q, want Host header", head)
		}
		b, _ := io.ReadAll(body)
		if string(b) != "body content" {
			t.Fatalf("body = %q, want %q", b, "body content")
		}
	})

	t.Run("lf_only_separator", func(t *testing.T) {
		input := "Host: example.com\n\nbody"
		head, body, err := splitLegacyEvidence(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(head, "Host: example.com") {
			t.Fatalf("head = %q", head)
		}
		b, _ := io.ReadAll(body)
		if string(b) != "body" {
			t.Fatalf("body = %q", b)
		}
	})

	t.Run("headers_only_eof", func(t *testing.T) {
		input := "Host: example.com"
		head, body, err := splitLegacyEvidence(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if head != "Host: example.com" {
			t.Fatalf("head = %q", head)
		}
		b, _ := io.ReadAll(body)
		if len(b) != 0 {
			t.Fatalf("body = %q, want empty", b)
		}
	})

	t.Run("oversized_header_rejected", func(t *testing.T) {
		huge := strings.Repeat("X", 1<<20+100)
		_, _, err := splitLegacyEvidence(strings.NewReader(huge))
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v, want 'too large'", err)
		}
	})
}

func TestSplitLegacyHTTP(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantHead string
		wantBody string
	}{
		{"crlf", "GET / HTTP/1.1\r\nHost: x\r\n\r\nbody", "GET / HTTP/1.1\r\nHost: x", "body"},
		{"lf_only", "GET / HTTP/1.1\nHost: x\n\nbody", "GET / HTTP/1.1\nHost: x", "body"},
		{"no_body", "GET / HTTP/1.1\r\nHost: x", "GET / HTTP/1.1\r\nHost: x", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, body := splitLegacyHTTP([]byte(tc.input))
			if head != tc.wantHead {
				t.Fatalf("head = %q, want %q", head, tc.wantHead)
			}
			if string(body) != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

func TestValidateHookDef(t *testing.T) {
	validReqSrc := `package hook
func OnRequest(req *Request) error { return nil }`
	validRespSrc := `package hook
func OnResponse(req *Request, resp *Response) error { return nil }`

	t.Run("valid_request_hook", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:  "test-hook",
			Phase: rewrite.PhaseRequest,
			Source: validReqSrc,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid_response_hook", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:  "resp-hook",
			Phase: rewrite.PhaseResponse,
			Source: validRespSrc,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty_name", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:   "",
			Phase:  rewrite.PhaseRequest,
			Source: validReqSrc,
		})
		if err == nil || !strings.Contains(err.Error(), "name") {
			t.Fatalf("err = %v, want name error", err)
		}
	})

	t.Run("whitespace_name", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:   "   ",
			Phase:  rewrite.PhaseRequest,
			Source: validReqSrc,
		})
		if err == nil {
			t.Fatal("expected error for whitespace-only name")
		}
	})

	t.Run("invalid_phase", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:   "bad-phase",
			Phase:  rewrite.Phase("invalid"),
			Source: validReqSrc,
		})
		if err == nil || !strings.Contains(err.Error(), "phase") {
			t.Fatalf("err = %v, want phase error", err)
		}
	})

	t.Run("invalid_source", func(t *testing.T) {
		err := ValidateHookDef(HookDef{
			Name:   "bad-src",
			Phase:  rewrite.PhaseRequest,
			Source: "not valid go",
		})
		if err == nil {
			t.Fatal("expected error for bad source")
		}
	})
}

func TestDryRunHook(t *testing.T) {
	src := `package hook
func OnRequest(req *Request) error {
	req.SetHeader("X-Dry", "yes")
	return nil
}`
	def := HookDef{
		ID:    42,
		Name:  "dry-test",
		Phase: rewrite.PhaseRequest,
		Source: src,
	}
	u, _ := url.Parse("https://api.example.test/v1/test")
	in := rewrite.Input{
		Request: rewrite.Request{
			Method: "GET",
			URL:    u,
			Header: http.Header{},
		},
		Response: rewrite.Response{Status: 200, Header: http.Header{}},
	}

	res, err := DryRunHook(def, in, []byte("trace-key-32-bytes-long-0000000"))
	if err != nil {
		t.Fatalf("DryRunHook: %v", err)
	}
	if res.Request.Header.Get("X-Dry") != "yes" {
		t.Fatalf("header not set: %v", res.Request.Header)
	}

	t.Run("invalid_def_returns_error", func(t *testing.T) {
		bad := HookDef{Name: "", Phase: rewrite.PhaseRequest, Source: src}
		_, err := DryRunHook(bad, in, nil)
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("response_phase", func(t *testing.T) {
		respSrc := `package hook
func OnResponse(req *Request, resp *Response) error {
	resp.SetHeader("X-DryResp", "done")
	return nil
}`
		def := HookDef{ID: 43, Name: "dry-resp", Phase: rewrite.PhaseResponse, Source: respSrc}
		res, err := DryRunHook(def, in, nil)
		if err != nil {
			t.Fatalf("DryRunHook response: %v", err)
		}
		if res.Response.Header.Get("X-DryResp") != "done" {
			t.Fatalf("response header not set: %v", res.Response.Header)
		}
	})
}

func TestHookSetLenNilReceiver(t *testing.T) {
	var nilHS *HookSet
	if nilHS.Len(rewrite.PhaseRequest) != 0 {
		t.Fatal("nil HookSet Len should be 0")
	}
	if nilHS.Len(rewrite.PhaseResponse) != 0 {
		t.Fatal("nil HookSet Len should be 0")
	}
}

func TestHookSetLastErrorNilReceiver(t *testing.T) {
	var nilHS *HookSet
	if nilHS.LastError(1) != nil {
		t.Fatal("nil HookSet LastError should be nil")
	}
}

func TestRecordErrorMethods(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	e := &recordError{step: "connect", err: inner}
	if e.Error() != "connect: dial tcp: connection refused" {
		t.Fatalf("Error() = %q", e.Error())
	}
	if e.Unwrap() != inner {
		t.Fatal("Unwrap did not return inner error")
	}
}

func TestExchangeExistsAndReadEvidence(t *testing.T) {
	tr, _ := openTraffic(t)

	tr.record(newFlow("ev.example.com", "GET", "/test",
		[]byte("req-body"), []byte("resp-body")))
	id := onlyExchangeID(t, tr)

	exists, err := tr.ExchangeExists(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exchange to exist")
	}

	exists, err = tr.ExchangeExists(context.Background(), "nonexistent-id")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected nonexistent exchange to not exist")
	}

	t.Run("nil_store", func(t *testing.T) {
		var nilTr *Traffic
		_, err := nilTr.ExchangeExists(context.Background(), id)
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("read_evidence_round_trip", func(t *testing.T) {
		var seen int
		err := tr.ReadEvidence(context.Background(), []string{id}, func(e EvidenceExchange) error {
			seen++
			if e.ID != id {
				t.Fatalf("evidence ID = %q, want %q", e.ID, id)
			}
			if e.Method != "GET" {
				t.Fatalf("method = %q", e.Method)
			}
			reqBody, _ := io.ReadAll(e.Request)
			if !strings.Contains(string(reqBody), "req-body") {
				t.Fatalf("request body = %q", reqBody)
			}
			respBody, _ := io.ReadAll(e.Response)
			if !strings.Contains(string(respBody), "resp-body") {
				t.Fatalf("response body = %q", respBody)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen != 1 {
			t.Fatalf("seen = %d, want 1", seen)
		}
	})

	t.Run("read_evidence_nil_store", func(t *testing.T) {
		var nilTr *Traffic
		err := nilTr.ReadEvidence(context.Background(), []string{id}, func(EvidenceExchange) error { return nil })
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("read_evidence_cancelled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := tr.ReadEvidence(ctx, []string{id}, func(EvidenceExchange) error { return nil })
		if err == nil {
			t.Fatal("expected context error")
		}
	})
}

func TestRawUDPDatagramGetAndCount(t *testing.T) {
	tr, _ := openTraffic(t)

	ts := time.Now()
	id, err := tr.RecordRawUDPDatagram(RawUDPCapture{
		AssociationID: "assoc-1",
		Host:          "udp.example.com",
		Port:          53,
		Direction:     "c2s",
		Timestamp:     ts,
		Payload:       []byte("hello dns"),
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("get_existing", func(t *testing.T) {
		d, err := tr.GetRawUDPDatagram(id)
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			t.Fatal("expected datagram, got nil")
		}
		if d.Host != "udp.example.com" || d.Port != 53 || d.Direction != "c2s" {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("get_not_found", func(t *testing.T) {
		d, err := tr.GetRawUDPDatagram("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if d != nil {
			t.Fatalf("expected nil, got %+v", d)
		}
	})

	t.Run("count_unfiltered", func(t *testing.T) {
		n, err := tr.CountRawUDPDatagrams("", "")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
	})

	t.Run("count_by_assoc", func(t *testing.T) {
		n, err := tr.CountRawUDPDatagrams("assoc-1", "")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
		n, err = tr.CountRawUDPDatagrams("nonexistent", "")
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("count = %d, want 0", n)
		}
	})

	t.Run("count_by_host", func(t *testing.T) {
		n, err := tr.CountRawUDPDatagrams("", "udp.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
	})

	t.Run("nil_store", func(t *testing.T) {
		var nilTr *Traffic
		_, err := nilTr.GetRawUDPDatagram(id)
		if err == nil {
			t.Fatal("expected error for nil store")
		}
		_, err = nilTr.CountRawUDPDatagrams("", "")
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})
}

func TestHostsHaveNoExchanges(t *testing.T) {
	tr, _ := openTraffic(t)

	empty, err := tr.hostsHaveNoExchanges([]string{"nohost.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("expected true for absent hosts")
	}

	tr.record(newFlow("real.example.com", "GET", "/",
		[]byte("r"), []byte("b")))

	empty, err = tr.hostsHaveNoExchanges([]string{"real.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("expected false for host with exchanges")
	}

	empty, err = tr.hostsHaveNoExchanges([]string{"nohost.example.com", "real.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("expected false when any host has exchanges")
	}
}

func TestMapDialErrorToSOCKS5ReplyExtended(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want byte
	}{
		{"dns_error", &net.DNSError{Err: "no such host", Name: "x"}, socks5RepHostUnreachable},
		{"conn_refused", syscall.ECONNREFUSED, socks5RepConnectionRefused},
		{"net_unreachable", syscall.ENETUNREACH, socks5RepNetworkUnreachable},
		{"host_unreachable", syscall.EHOSTUNREACH, socks5RepHostUnreachable},
		{"deadline_exceeded", context.DeadlineExceeded, socks5RepTTLExpired},
		{"string_conn_refused", errors.New("connection refused by remote"), socks5RepConnectionRefused},
		{"string_net_unreachable", errors.New("network is unreachable"), socks5RepNetworkUnreachable},
		{"string_no_route", errors.New("no route to host"), socks5RepHostUnreachable},
		{"string_no_such_host", errors.New("no such host"), socks5RepHostUnreachable},
		{"string_host_unreachable", errors.New("host is unreachable"), socks5RepHostUnreachable},
		{"generic", errors.New("something else entirely"), socks5RepGeneralFailure},
		{"wrapped_conn_refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), socks5RepConnectionRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapDialErrorToSOCKS5Reply(tc.err)
			if got != tc.want {
				t.Fatalf("got 0x%02x, want 0x%02x", got, tc.want)
			}
		})
	}
}

func TestIsLoopbackRemoteExtended(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want bool
	}{
		{"tcp_loopback4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80}, true},
		{"tcp_loopback6", &net.TCPAddr{IP: net.IPv6loopback, Port: 80}, true},
		{"tcp_nonloopback", &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 80}, false},
		{"tcp_nil_ip", &net.TCPAddr{Port: 80}, false},
		{"nil_addr", nil, false},
		{"udp_loopback", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80}, true},
		{"udp_nonloopback", &net.UDPAddr{IP: net.IPv4(192, 168, 1, 1), Port: 80}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isLoopbackRemote(tc.addr)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompileHooksDisabledSkipped(t *testing.T) {
	src := `package hook
func OnRequest(req *Request) error { return nil }`
	hs, errs := CompileHooks([]HookDef{
		{ID: 1, Name: "enabled", Enabled: true, Phase: rewrite.PhaseRequest, Source: src},
		{ID: 2, Name: "disabled", Enabled: false, Phase: rewrite.PhaseRequest, Source: src},
	}, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if hs.Len(rewrite.PhaseRequest) != 1 {
		t.Fatalf("compiled = %d, want 1 (disabled should be skipped)", hs.Len(rewrite.PhaseRequest))
	}
}

func TestCompileHooksInvalidReportsError(t *testing.T) {
	_, errs := CompileHooks([]HookDef{
		{ID: 1, Name: "bad", Enabled: true, Phase: rewrite.PhaseRequest, Source: "not go"},
	}, nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 load error, got %d", len(errs))
	}
	if _, ok := errs[1]; !ok {
		t.Fatalf("expected error for ID 1, got %v", errs)
	}
}

func TestCompileHooksPriorityOrder(t *testing.T) {
	src := `package hook
func OnRequest(req *Request) error { return nil }`
	hs, errs := CompileHooks([]HookDef{
		{ID: 3, Name: "low-pri", Enabled: true, Phase: rewrite.PhaseRequest, Priority: 10, Source: src},
		{ID: 1, Name: "high-pri", Enabled: true, Phase: rewrite.PhaseRequest, Priority: 1, Source: src},
		{ID: 2, Name: "mid-pri", Enabled: true, Phase: rewrite.PhaseRequest, Priority: 5, Source: src},
	}, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if hs.Len(rewrite.PhaseRequest) != 3 {
		t.Fatalf("compiled = %d, want 3", hs.Len(rewrite.PhaseRequest))
	}
}

func TestTamperHooksGetterAndRewriteRulesGetter(t *testing.T) {
	tr, _ := openTraffic(t)

	if got := tr.TamperHooks(); got != nil {
		t.Fatalf("fresh store TamperHooks = %v, want nil", got)
	}
	if got := tr.RewriteRules(); got != nil {
		t.Fatalf("fresh store RewriteRules = %v, want nil", got)
	}

	src := `package hook
func OnRequest(req *Request) error { return nil }`
	hs, _ := CompileHooks([]HookDef{
		{ID: 1, Name: "h", Enabled: true, Phase: rewrite.PhaseRequest, Source: src},
	}, nil)
	tr.SetTamperHooks(hs)
	if got := tr.TamperHooks(); got != hs {
		t.Fatal("TamperHooks did not return set value")
	}

	rs := rewrite.Compile(nil, rewrite.Options{})
	tr.SetRewriteRules(rs)
	if got := tr.RewriteRules(); got != rs {
		t.Fatal("RewriteRules did not return set value")
	}

	var nilTr *Traffic
	if nilTr.TamperHooks() != nil {
		t.Fatal("nil store TamperHooks should be nil")
	}
}

func TestSeedToolMetas(t *testing.T) {
	tools := SeedToolMetas()
	if len(tools) == 0 {
		t.Fatal("SeedToolMetas returned no tools")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	if !names["traffic_search"] {
		t.Fatal("missing traffic_search tool")
	}
}

func TestReadEvidenceWithBlobSpill(t *testing.T) {
	tr, _ := openTraffic(t)

	largeReq := strings.Repeat("A", 128*1024)
	largeResp := strings.Repeat("B", 128*1024)
	tr.record(newFlow("blob.example.com", "POST", "/big",
		[]byte(largeReq), []byte(largeResp)))

	id := onlyExchangeID(t, tr)

	var reqBlob, respBlob string
	err := tr.DB().QueryRow(`SELECT COALESCE(req_blob,''), COALESCE(resp_blob,'') FROM exchange_bodies WHERE id=?`, id).
		Scan(&reqBlob, &respBlob)
	if err != nil {
		t.Fatal(err)
	}

	var seen int
	err = tr.ReadEvidence(context.Background(), []string{id}, func(e EvidenceExchange) error {
		seen++
		reqData, _ := io.ReadAll(e.Request)
		respData, _ := io.ReadAll(e.Response)
		if len(reqData) < 128*1024 {
			t.Fatalf("request body too small: %d", len(reqData))
		}
		if len(respData) < 128*1024 {
			t.Fatalf("response body too small: %d", len(respData))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("seen = %d, want 1", seen)
	}
}

func TestReadEvidenceConsumeError(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("err.example.com", "GET", "/", []byte("r"), []byte("b")))
	id := onlyExchangeID(t, tr)

	wantErr := errors.New("callback failure")
	err := tr.ReadEvidence(context.Background(), []string{id}, func(EvidenceExchange) error {
		return wantErr
	})
	if err == nil || !strings.Contains(err.Error(), "callback failure") {
		t.Fatalf("err = %v, want wrapped callback failure", err)
	}
}

func TestReadEvidenceNotFound(t *testing.T) {
	tr, _ := openTraffic(t)
	err := tr.ReadEvidence(context.Background(), []string{"no-such-id"}, func(EvidenceExchange) error {
		t.Fatal("should not be called")
		return nil
	})
	if err == nil {
		t.Fatal("expected error for missing exchange")
	}
}
