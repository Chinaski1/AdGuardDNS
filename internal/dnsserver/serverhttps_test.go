package dnsserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/AdguardTeam/AdGuardDNS/internal/dnsserver"
	"github.com/AdguardTeam/AdGuardDNS/internal/dnsserver/dnsservertest"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/golibs/testutil"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestServerHTTPS_integration_serveRequests(t *testing.T) {
	testCases := []struct {
		name          string
		method        string
		tls           bool
		json          bool
		reqWireFormat bool
		http3Enabled  bool
	}{{
		name:   "doh_get_wireformat",
		method: http.MethodGet,
		tls:    true,
		json:   false,
	}, {
		name:   "doh_post_wireformat",
		method: http.MethodPost,
		tls:    true,
		json:   false,
	}, {
		name:   "doh_plain_get_wireformat",
		method: http.MethodGet,
		tls:    false,
		json:   false,
	}, {
		name:   "doh_plain_post_wireformat",
		method: http.MethodPost,
		tls:    false,
		json:   false,
	}, {
		name:   "doh_get_json",
		method: http.MethodGet,
		tls:    true,
		json:   true,
	}, {
		name:   "doh_post_json",
		method: http.MethodPost,
		tls:    true,
		json:   true,
	}, {
		name:   "doh_plain_get_json",
		method: http.MethodGet,
		tls:    false,
		json:   true,
	}, {
		name:   "doh_plain_post_json",
		method: http.MethodPost,
		tls:    false,
		json:   true,
	}, {
		name:          "doh_get_json_wireformat",
		method:        http.MethodGet,
		tls:           true,
		json:          true,
		reqWireFormat: true,
	}, {
		name:          "doh_post_json_wireformat",
		method:        http.MethodPost,
		tls:           true,
		json:          true,
		reqWireFormat: true,
	}, {
		name:         "doh3_get_wireformat",
		method:       http.MethodGet,
		tls:          true,
		json:         false,
		http3Enabled: true,
	}, {
		name:         "doh3_post_wireformat",
		method:       http.MethodPost,
		tls:          true,
		json:         false,
		http3Enabled: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tlsConfig := dnsservertest.CreateServerTLSConfig("example.org")
			srv, err := dnsservertest.RunLocalHTTPSServer(
				dnsservertest.DefaultHandler(),
				tlsConfig,
				nil,
			)
			require.NoError(t, err)

			testutil.CleanupAndRequireSuccess(t, func() (err error) {
				return srv.Shutdown(context.Background())
			})

			// Create a test message
			req := new(dns.Msg)
			req.Id = dns.Id()
			req.RecursionDesired = true
			name := "example.org."
			req.Question = []dns.Question{
				{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET},
			}

			var resp *dns.Msg
			addr := srv.LocalTCPAddr()
			if tc.http3Enabled {
				addr = srv.LocalUDPAddr()
			}

			resp = mustDoHReq(t, addr, tlsConfig, tc.method, tc.json, tc.reqWireFormat, req)
			require.True(t, resp.Response)

			// EDNS0 padding is only present when request also has padding opt.
			paddingOpt := dnsservertest.FindEDNS0Option[*dns.EDNS0_PADDING](resp)
			require.Nil(t, paddingOpt)
		})
	}
}

func TestServerHTTPS_integration_nonDNSHandler(t *testing.T) {
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	srv, err := dnsservertest.RunLocalHTTPSServer(
		dnsservertest.DefaultHandler(),
		nil,
		testHandler,
	)
	require.NoError(t, err)

	testutil.CleanupAndRequireSuccess(t, func() (err error) {
		return srv.Shutdown(context.Background())
	})

	var resp *http.Response
	resp, err = http.Get(fmt.Sprintf("http://%s/test", srv.LocalTCPAddr()))
	defer log.OnCloserError(resp.Body, log.DEBUG)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDNSMsgToJSONMsg(t *testing.T) {
	m := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id:                 dns.Id(),
			Response:           true,
			Authoritative:      true,
			RecursionAvailable: true,
			RecursionDesired:   true,
			AuthenticatedData:  true,
			CheckingDisabled:   true,
			Rcode:              dns.RcodeSuccess,
		},
		Question: []dns.Question{
			{
				Name:   "example.org",
				Qtype:  dns.TypeA,
				Qclass: dns.ClassINET,
			},
		},
		Answer: []dns.RR{
			&dns.A{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    200,
				},
				A: net.ParseIP("127.0.0.1"),
			},
			&dns.AAAA{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeAAAA,
					Class:  dns.ClassINET,
					Ttl:    200,
				},
				AAAA: net.ParseIP("2000::"),
			},
			&dns.TXT{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeTXT,
					Class:  dns.ClassINET,
					Ttl:    100,
				},
				Txt: []string{
					"value1",
					"value2",
				},
			},
			&dns.CNAME{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeCNAME,
					Class:  dns.ClassINET,
					Ttl:    100,
				},
				Target: "example.com",
			},
			&dns.SVCB{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeHTTPS,
					Class:  dns.ClassINET,
					Ttl:    100,
				},
				Target: "example.com",
				Value: []dns.SVCBKeyValue{
					&dns.SVCBAlpn{
						Alpn: []string{"h2", "h3"},
					},
					&dns.SVCBECHConfig{
						ECH: []byte{1, 2},
					},
					&dns.SVCBIPv4Hint{
						Hint: []net.IP{
							net.ParseIP("127.0.0.1"),
							net.ParseIP("127.0.0.2"),
						},
					},
					&dns.SVCBIPv6Hint{
						Hint: []net.IP{
							net.ParseIP("2000::"),
							net.ParseIP("2001::"),
						},
					},
				},
			},
		},
		Extra: []dns.RR{
			&dns.AAAA{
				Hdr: dns.RR_Header{
					Name:   "example.org",
					Rrtype: dns.TypeAAAA,
					Class:  dns.ClassCHAOS,
					Ttl:    200,
				},
				AAAA: net.ParseIP("2000::"),
			},
		},
	}

	jsonMsg := dnsserver.DNSMsgToJSONMsg(m)
	require.NotNil(t, jsonMsg)
	require.Equal(t, dns.RcodeSuccess, jsonMsg.Status)
	require.True(t, jsonMsg.RecursionDesired)
	require.True(t, jsonMsg.AuthenticatedData)
	require.True(t, jsonMsg.RecursionAvailable)
	require.True(t, jsonMsg.AuthenticatedData)
	require.True(t, jsonMsg.CheckingDisabled)
	require.False(t, jsonMsg.Truncated)
	require.Equal(t, []dnsserver.JSONQuestion{{
		Name: "example.org",
		Type: dns.TypeA,
	}}, jsonMsg.Question)
	require.Equal(t, []dnsserver.JSONAnswer{{
		Name:  "example.org",
		Type:  dns.TypeA,
		Class: dns.ClassINET,
		TTL:   200,
		Data:  "127.0.0.1",
	}, {
		Name:  "example.org",
		Type:  dns.TypeAAAA,
		Class: dns.ClassINET,
		TTL:   200,
		Data:  "2000::",
	}, {
		Name:  "example.org",
		Type:  dns.TypeTXT,
		Class: dns.ClassINET,
		TTL:   100,
		Data:  `"value1" "value2"`,
	}, {
		Name:  "example.org",
		Type:  dns.TypeCNAME,
		Class: dns.ClassINET,
		TTL:   100,
		Data:  "example.com",
	}, {
		Name:  "example.org",
		Type:  dns.TypeHTTPS,
		Class: dns.ClassINET,
		TTL:   100,
		Data:  `0 example.com alpn="h2,h3" ech="AQI=" ipv4hint="127.0.0.1,127.0.0.2" ipv6hint="2000::,2001::"`,
	}}, jsonMsg.Answer)
	require.Equal(t, []dnsserver.JSONAnswer{{
		Name:  "example.org",
		Type:  dns.TypeAAAA,
		Class: dns.ClassCHAOS,
		TTL:   200,
		Data:  "2000::",
	}}, jsonMsg.Extra)
}

func TestServerHTTPS_integration_ENDS0Padding(t *testing.T) {
	tlsConfig := dnsservertest.CreateServerTLSConfig("example.org")
	srv, err := dnsservertest.RunLocalHTTPSServer(
		dnsservertest.DefaultHandler(),
		tlsConfig,
		nil,
	)
	require.NoError(t, err)

	testutil.CleanupAndRequireSuccess(t, func() (err error) {
		return srv.Shutdown(context.Background())
	})

	req := dnsservertest.CreateMessage("example.org.", dns.TypeA)
	req.Extra = []dns.RR{dnsservertest.NewEDNS0Padding(req.Len(), dns.DefaultMsgSize)}
	addr := srv.LocalTCPAddr()

	resp := mustDoHReq(t, addr, tlsConfig, http.MethodGet, false, false, req)
	require.True(t, resp.Response)

	paddingOpt := dnsservertest.FindEDNS0Option[*dns.EDNS0_PADDING](resp)
	require.NotNil(t, paddingOpt)
	require.NotEmpty(t, paddingOpt.Padding)
}

func mustDoHReq(
	t testing.TB,
	httpsAddr net.Addr,
	tlsConfig *tls.Config,
	method string,
	json bool,
	requestWireformat bool,
	req *dns.Msg,
) (resp *dns.Msg) {
	t.Helper()

	client, err := createDoHClient(httpsAddr, tlsConfig)
	require.NoError(t, err)

	proto := "https"
	if tlsConfig == nil {
		proto = "http"
	}

	var httpReq *http.Request
	if json {
		httpReq, err = createJSONRequest(proto, method, requestWireformat, req)
	} else {
		httpReq, err = createDoHRequest(proto, method, req)
	}
	require.NoError(t, err)

	httpResp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer log.OnCloserError(httpResp.Body, log.DEBUG)

	if tlsConfig != nil && !httpResp.ProtoAtLeast(2, 0) {
		t.Fatal(fmt.Errorf("protocol is too old: %s", httpResp.Proto))
	}

	body, err := io.ReadAll(httpResp.Body)
	require.NoError(t, err)

	if json && !requestWireformat {
		resp, err = unpackJSONMsg(body)
	} else {
		resp, err = unpackDoHMsg(body)
	}
	require.NoError(t, err)
	require.NotNil(t, resp)

	return resp
}

func createDoHClient(httpsAddr net.Addr, tlsConfig *tls.Config) (client *http.Client, err error) {
	if dnsserver.NetworkFromAddr(httpsAddr) == dnsserver.NetworkUDP {
		return createDoH3Client(httpsAddr, tlsConfig)
	}

	return createDoH2Client(httpsAddr, tlsConfig)
}

func createDoH2Client(httpsAddr net.Addr, tlsConfig *tls.Config) (client *http.Client, err error) {
	if tlsConfig != nil {
		tlsConfig = tlsConfig.Clone()
		tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	}

	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Route request to the DoH server address
		return dialer.DialContext(ctx, network, httpsAddr.String())
	}
	transport := &http.Transport{
		TLSClientConfig:    tlsConfig,
		DisableCompression: true,
		DialContext:        dialContext,
		ForceAttemptHTTP2:  true,
	}
	if tlsConfig != nil {
		err = http2.ConfigureTransport(transport)
		if err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}, nil
}

func createDoH3Client(httpsAddr net.Addr, tlsConfig *tls.Config) (client *http.Client, err error) {
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{"h3"}

	transport := &http3.RoundTripper{
		DisableCompression: true,
		Dial: func(
			ctx context.Context,
			_ string,
			tlsCfg *tls.Config,
			cfg *quic.Config,
		) (c quic.EarlyConnection, e error) {
			return quic.DialAddrEarlyContext(ctx, httpsAddr.String(), tlsCfg, cfg)
		},
		TLSClientConfig: tlsConfig,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}, nil
}

func createDoHRequest(proto, method string, msg *dns.Msg) (r *http.Request, err error) {
	// Prepare message
	var buf []byte
	buf, err = msg.Pack()
	if err != nil {
		return nil, err
	}

	// Prepare the *http.Request with the DNS message.
	requestURL := proto + "://test.local" + dnsserver.PathDoH
	if method == http.MethodPost {
		bb := bytes.NewBuffer(buf)
		r, err = http.NewRequest(method, requestURL, bb)
	} else {
		requestURL = requestURL + "?dns=" + base64.RawURLEncoding.EncodeToString(buf)
		r, err = http.NewRequest(method, requestURL, nil)
	}

	if err != nil {
		return nil, err
	}

	r.Header.Set("Content-Type", dnsserver.MimeTypeDoH)
	r.Header.Set("Accept", dnsserver.MimeTypeDoH)

	return r, nil
}

func createJSONRequest(
	proto string,
	method string,
	requestWireformat bool,
	msg *dns.Msg,
) (r *http.Request, err error) {
	q := url.Values{}
	q.Add("name", msg.Question[0].Name)
	q.Add("type", dns.TypeToString[msg.Question[0].Qtype])
	q.Add("qc", dns.ClassToString[msg.Question[0].Qclass])
	q.Add("cd", strconv.FormatBool(msg.CheckingDisabled))
	if requestWireformat {
		q.Add("ct", dnsserver.MimeTypeDoH)
	}

	if opt := msg.IsEdns0(); opt != nil {
		q.Add("do", strconv.FormatBool(opt.Do()))
	}

	requestURL := fmt.Sprintf("%s://test.local%s?%s", proto, dnsserver.PathJSON, q.Encode())
	r, err = http.NewRequest(method, requestURL, nil)

	if err != nil {
		return nil, err
	}

	r.Header.Set("Content-Type", dnsserver.MimeTypeJSON)
	r.Header.Set("Accept", dnsserver.MimeTypeJSON)

	return r, err
}

func unpackJSONMsg(b []byte) (m *dns.Msg, err error) {
	var jsonMsg *dnsserver.JSONMsg
	err = json.Unmarshal(b, &jsonMsg)
	if err != nil {
		return nil, err
	}

	m = &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Response:           true,
			Rcode:              jsonMsg.Status,
			Truncated:          jsonMsg.Truncated,
			RecursionDesired:   jsonMsg.RecursionDesired,
			RecursionAvailable: jsonMsg.RecursionAvailable,
			CheckingDisabled:   jsonMsg.CheckingDisabled,
			AuthenticatedData:  jsonMsg.AuthenticatedData,
		},
	}

	for _, q := range jsonMsg.Question {
		m.Question = append(m.Question, dns.Question{
			Name:  q.Name,
			Qtype: q.Type,
		})
	}

	for _, a := range jsonMsg.Answer {
		rrHeader := dns.RR_Header{
			Name:   a.Name,
			Ttl:    a.TTL,
			Rrtype: a.Type,
		}

		var rr dns.RR

		switch a.Type {
		case dns.TypeA:
			rr = &dns.A{
				Hdr: rrHeader,
				A:   net.ParseIP(a.Data),
			}
		case dns.TypeAAAA:
			rr = &dns.AAAA{
				Hdr:  rrHeader,
				AAAA: net.ParseIP(a.Data),
			}
		default:
			panic("we do not support other RR types in this test")
		}

		m.Answer = append(m.Answer, rr)
	}

	return m, nil
}

func unpackDoHMsg(b []byte) (m *dns.Msg, err error) {
	m = &dns.Msg{}
	err = m.Unpack(b)
	return m, err
}
