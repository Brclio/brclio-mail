package mailcore

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestComposeAndParseMultipartMessage(t *testing.T) {
	raw, err := Compose(ComposeRequest{
		From: "Alice <alice@example.com>", To: []string{"Bob <bob@example.net>"}, CC: []string{"team@example.net"},
		Subject: "你好，私人邮局", TextBody: "纯文本正文", HTMLBody: `<p onclick="alert(1)">安全正文<script>alert(2)</script><img src="https://tracker.invalid/pixel"></p>`,
		Attachments: []ComposeAttachment{{Filename: "说明.txt", ContentType: "text/plain", ContentBase64: base64.StdEncoding.EncodeToString([]byte("attachment-body"))}},
	}, "mail.example.com", time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("message does not use CRLF")
	}
	parsed, attachments, err := Parse(raw, Envelope{From: "alice@example.com", To: []string{"bob@example.net"}, Direction: "outbound"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject != "你好，私人邮局" || parsed.TextBody != "纯文本正文" {
		t.Fatalf("unexpected parsed message: %#v", parsed)
	}
	if strings.Contains(strings.ToLower(parsed.HTMLBody), "script") || strings.Contains(parsed.HTMLBody, "tracker.invalid") || strings.Contains(parsed.HTMLBody, "onclick") {
		t.Fatalf("unsafe HTML remained: %s", parsed.HTMLBody)
	}
	if len(attachments) != 1 || attachments[0].Filename != "说明.txt" || string(attachments[0].Content) != "attachment-body" {
		t.Fatalf("unexpected attachments: %#v", attachments)
	}
}

func TestComposeRejectsHeaderInjectionAndBadRecipient(t *testing.T) {
	_, err := Compose(ComposeRequest{From: "alice@example.com", To: []string{"bob@example.net"}, Subject: "ok\r\nBcc: attacker@example.net", TextBody: "x"}, "mail.example.com", time.Now())
	if err == nil {
		t.Fatal("header injection was accepted")
	}
	_, err = Compose(ComposeRequest{From: "alice@example.com", To: []string{"not an address"}, Subject: "x", TextBody: "x"}, "mail.example.com", time.Now())
	if err == nil {
		t.Fatal("invalid recipient was accepted")
	}
}

func TestComposeAttachmentLimitNeverExceedsParserPartLimit(t *testing.T) {
	if maxComposeAttachmentBytes > maxDecodedPartBytes {
		t.Fatalf("compose attachment limit %d exceeds parser part limit %d", maxComposeAttachmentBytes, maxDecodedPartBytes)
	}
}

func TestComposeRejectsTooManyAttachmentsBeforeBuildingThem(t *testing.T) {
	attachments := make([]ComposeAttachment, maxAttachments+1)
	for index := range attachments {
		attachments[index] = ComposeAttachment{Filename: fmt.Sprintf("part-%d.txt", index), ContentBase64: base64.StdEncoding.EncodeToString([]byte("x"))}
	}
	_, err := Compose(ComposeRequest{From: "alice@example.com", To: []string{"bob@example.net"}, Attachments: attachments}, "mail.example.com", time.Now())
	if err == nil || !strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("compose accepted too many attachments: %v", err)
	}
}

func TestDraftMayHaveNoRecipients(t *testing.T) {
	_, err := Compose(ComposeRequest{From: "alice@example.com", Subject: "unfinished", AllowNoRecipients: true}, "mail.example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsExcessiveMIMEParts(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("From: sender@example.net\r\nTo: alice@example.com\r\n")
	raw.WriteString("Content-Type: multipart/mixed; boundary=parts\r\n\r\n")
	for index := 0; index < maxMIMEParts; index++ {
		fmt.Fprintf(&raw, "--parts\r\nContent-Type: text/plain\r\n\r\n%d\r\n", index)
	}
	raw.WriteString("--parts--\r\n")

	_, _, err := Parse([]byte(raw.String()), Envelope{From: "sender@example.net", To: []string{"alice@example.com"}, Direction: "inbound"})
	if err == nil || !strings.Contains(err.Error(), "too many MIME parts") {
		t.Fatalf("excessive MIME part count was accepted: %v", err)
	}
}

func TestParseRejectsExcessiveMIMEDepth(t *testing.T) {
	body := "Content-Type: text/plain\r\n\r\nhello\r\n"
	for depth := 0; depth <= maxMIMEDepth; depth++ {
		boundary := fmt.Sprintf("nested-%d", depth)
		body = fmt.Sprintf("Content-Type: multipart/mixed; boundary=%s\r\n\r\n--%s\r\n%s--%s--\r\n", boundary, boundary, body, boundary)
	}
	raw := "From: sender@example.net\r\nTo: alice@example.com\r\n" + body

	_, _, err := Parse([]byte(raw), Envelope{From: "sender@example.net", To: []string{"alice@example.com"}, Direction: "inbound"})
	if err == nil || !strings.Contains(err.Error(), "nesting is too deep") {
		t.Fatalf("excessive MIME nesting was accepted: %v", err)
	}
}

func TestParseRejectsTooManyAttachments(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("From: sender@example.net\r\nTo: alice@example.com\r\n")
	raw.WriteString("Content-Type: multipart/mixed; boundary=attachments\r\n\r\n")
	for index := 0; index <= maxAttachments; index++ {
		fmt.Fprintf(&raw, "--attachments\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=part-%d.bin\r\n\r\nx\r\n", index)
	}
	raw.WriteString("--attachments--\r\n")

	_, _, err := Parse([]byte(raw.String()), Envelope{From: "sender@example.net", To: []string{"alice@example.com"}, Direction: "inbound"})
	if err == nil || !strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("excessive attachment count was accepted: %v", err)
	}
}
