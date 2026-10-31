package service

import (
	"bufio"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"strings"
	"testing"
)

// smtpSink accepts one message the way a plain SMTP server would and
// returns its DATA, headers and body.
func smtpSink(t *testing.T) (addr string, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
		say("220 sink")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 sink")
			case cmd == "DATA":
				say("354 go on")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				out <- data.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), out
}

// A subject comes from settings and names (site_name, a product's
// name): a line break in one stays inside the Subject header instead
// of starting a header of its own, and non-ASCII is encoded.
func TestDeliverKeepsTheSubjectOneHeader(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"Welcome to Acme\r\nBcc: someone@example.com", "Welcome to Acme Bcc: someone@example.com"},
		{"Welcome to Acme\nX-Injected: 1", "Welcome to Acme X-Injected: 1"},
		{"Café Pro — payment recovered", "Café Pro — payment recovered"},
		{"Welcome to Acme", "Welcome to Acme"},
	} {
		addr, got := smtpSink(t)
		host, port, _ := net.SplitHostPort(addr)
		svc := &EmailService{logger: slog.Default()}
		cfg := resolvedConfig{provider: "smtp", from: "noreply@example.com", host: host, port: port}
		if err := svc.deliver(cfg, "u@example.com", tc.subject, "<p>hi</p>"); err != nil {
			t.Fatalf("%q: deliver: %v", tc.subject, err)
		}
		msg, err := mail.ReadMessage(strings.NewReader(<-got))
		if err != nil {
			t.Fatalf("%q: message does not parse: %v", tc.subject, err)
		}
		for _, h := range []string{"Bcc", "X-Injected"} {
			if v := msg.Header.Get(h); v != "" {
				t.Errorf("%q: injected header %s: %q", tc.subject, h, v)
			}
		}
		raw := msg.Header.Get("Subject")
		for _, r := range raw {
			if r > 127 {
				t.Errorf("%q: Subject header is not ASCII: %q", tc.subject, raw)
				break
			}
		}
		decoded, err := new(mime.WordDecoder).DecodeHeader(raw)
		if err != nil || decoded != tc.want {
			t.Errorf("%q: Subject reads %q (err %v), want %q", tc.subject, decoded, err, tc.want)
		}
	}
}
