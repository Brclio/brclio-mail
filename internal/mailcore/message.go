package mailcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	messageMail "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
)

const (
	maxDecodedPartBytes       = 20 * 1024 * 1024
	maxComposeAttachmentBytes = 18 * 1024 * 1024
	maxMIMEParts              = 1000
	maxMIMEDepth              = 30
	maxAttachments            = 100
)

var whitespace = regexp.MustCompile(`\s+`)

type Envelope struct {
	From      string
	To        []string
	Direction string
}

type ComposeRequest struct {
	DraftID           string              `json:"draftId,omitempty"`
	From              string              `json:"from"`
	To                []string            `json:"to"`
	CC                []string            `json:"cc"`
	BCC               []string            `json:"bcc"`
	ReplyTo           string              `json:"replyTo,omitempty"`
	Subject           string              `json:"subject"`
	TextBody          string              `json:"body"`
	HTMLBody          string              `json:"htmlBody,omitempty"`
	InReplyTo         string              `json:"inReplyTo,omitempty"`
	References        []string            `json:"references,omitempty"`
	Attachments       []ComposeAttachment `json:"attachments,omitempty"`
	AllowNoRecipients bool                `json:"-"`
}

type ComposeAttachment struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
}

func Parse(raw []byte, envelope Envelope) (store.Message, []store.Attachment, error) {
	if len(raw) == 0 {
		return store.Message{}, nil, errors.New("empty message")
	}
	entity, err := message.ReadWithOptions(bytes.NewReader(raw), &message.ReadOptions{MaxHeaderBytes: 1024 * 1024})
	if err != nil && entity == nil {
		return store.Message{}, nil, fmt.Errorf("parse message headers: %w", err)
	}
	header := messageMail.Header{Header: entity.Header}

	result := store.Message{Raw: append([]byte(nil), raw...), SizeBytes: int64(len(raw)), Direction: envelope.Direction,
		EnvelopeFrom: normalizeAddress(envelope.From), EnvelopeTo: normalizeAddresses(envelope.To), ReceivedAt: time.Now().UTC(),
		TransportStatus: "received"}
	result.Subject, _ = entity.Header.Text("Subject")
	result.Subject = cleanHeader(result.Subject)
	result.RFCMessageID, _ = header.MessageID()
	if result.RFCMessageID == "" {
		result.RFCMessageID = security.TokenHash(string(raw))[:32] + "@brclio.local"
	}
	result.HeaderFrom = firstAddress(header, "From")
	result.HeaderTo = addressList(header, "To")
	result.HeaderCC = addressList(header, "Cc")
	result.HeaderBCC = addressList(header, "Bcc")
	result.ReplyTo = firstAddress(header, "Reply-To")
	if sentAt, dateErr := header.Date(); dateErr == nil {
		sentAt = sentAt.UTC()
		result.SentAt = &sentAt
	}
	result.ThreadKey = threadKey(result.Subject, entity.Header.Get("References"), entity.Header.Get("In-Reply-To"))

	var textParts, htmlParts []string
	var attachments []store.Attachment
	partCount := 0
	walkErr := entity.Walk(func(path []int, part *message.Entity, partErr error) error {
		if partErr != nil && part == nil {
			return partErr
		}
		partCount++
		if partCount > maxMIMEParts {
			return errors.New("message has too many MIME parts")
		}
		if len(path) > maxMIMEDepth {
			return errors.New("message MIME nesting is too deep")
		}
		mediaType, params, _ := part.Header.ContentType()
		disposition, dispositionParams, _ := part.Header.ContentDisposition()
		filename := dispositionParams["filename"]
		if filename == "" {
			filename = params["name"]
		}
		filename = safeFilename(filename)
		if strings.HasPrefix(mediaType, "multipart/") {
			return nil
		}
		body, readErr := readLimited(part.Body, maxDecodedPartBytes)
		if readErr != nil {
			return readErr
		}
		isAttachment := filename != "" || strings.EqualFold(disposition, "attachment") || strings.EqualFold(disposition, "inline")
		if !isAttachment && strings.EqualFold(mediaType, "text/plain") {
			textParts = append(textParts, strings.TrimSpace(string(body)))
			return nil
		}
		if !isAttachment && strings.EqualFold(mediaType, "text/html") {
			htmlParts = append(htmlParts, sanitizeHTML(string(body)))
			return nil
		}
		if filename == "" {
			filename = fmt.Sprintf("part-%d%s", len(attachments)+1, extensionForType(mediaType))
		}
		if len(attachments) >= maxAttachments {
			return errors.New("message has too many attachments")
		}
		sum := sha256.Sum256(body)
		attachmentID, idErr := security.NewID("att")
		if idErr != nil {
			return idErr
		}
		attachments = append(attachments, store.Attachment{ID: attachmentID, Filename: filename,
			ContentType: mediaType, ContentID: strings.Trim(part.Header.Get("Content-ID"), "<>"),
			Disposition: disposition, SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Content: body})
		return nil
	})
	if walkErr != nil {
		return store.Message{}, nil, fmt.Errorf("parse message body: %w", walkErr)
	}
	result.TextBody = strings.TrimSpace(strings.Join(textParts, "\n\n"))
	result.HTMLBody = strings.TrimSpace(strings.Join(htmlParts, "\n"))
	if result.TextBody == "" && result.HTMLBody != "" {
		result.TextBody = htmlToText(result.HTMLBody)
	}
	result.Snippet = snippet(result.TextBody)
	result.AttachmentCount = len(attachments)
	return result, attachments, nil
}

func Compose(req ComposeRequest, hostname string, now time.Time) ([]byte, error) {
	recipients := append(append(append([]string{}, req.To...), req.CC...), req.BCC...)
	if !req.AllowNoRecipients {
		if err := validateRecipients(recipients); err != nil {
			return nil, err
		}
	} else if len(recipients) > 0 {
		if err := validateRecipients(recipients); err != nil {
			return nil, err
		}
	}
	from, err := mail.ParseAddress(req.From)
	if err != nil {
		return nil, fmt.Errorf("invalid from address: %w", err)
	}
	if strings.ContainsAny(req.Subject, "\r\n") {
		return nil, errors.New("invalid subject")
	}
	if len(req.Subject) > 998 {
		return nil, errors.New("subject is too long")
	}
	if hostname == "" {
		hostname = "localhost"
	}
	messageID, err := security.RandomToken(18)
	if err != nil {
		return nil, err
	}
	messageID = "<" + messageID + "@" + hostname + ">"

	var output bytes.Buffer
	headers := textproto.MIMEHeader{}
	headers.Set("From", from.String())
	if len(req.To) > 0 {
		headers.Set("To", formatAddressHeader(req.To))
	}
	if len(req.CC) > 0 {
		headers.Set("Cc", formatAddressHeader(req.CC))
	}
	if req.ReplyTo != "" {
		if _, err := mail.ParseAddress(req.ReplyTo); err != nil {
			return nil, fmt.Errorf("invalid reply-to: %w", err)
		}
		headers.Set("Reply-To", req.ReplyTo)
	}
	headers.Set("Subject", mime.QEncoding.Encode("UTF-8", req.Subject))
	headers.Set("Date", now.UTC().Format(time.RFC1123Z))
	headers.Set("Message-ID", messageID)
	headers.Set("MIME-Version", "1.0")
	headers.Set("User-Agent", "Brclio Mail/0.1")
	if req.InReplyTo != "" {
		headers.Set("In-Reply-To", cleanMessageID(req.InReplyTo))
	}
	if len(req.References) > 0 {
		refs := make([]string, 0, len(req.References))
		for _, ref := range req.References {
			if clean := cleanMessageID(ref); clean != "" {
				refs = append(refs, clean)
			}
		}
		if len(refs) > 0 {
			headers.Set("References", strings.Join(refs, " "))
		}
	}

	mixed := multipart.NewWriter(&output)
	headers.Set("Content-Type", fmt.Sprintf(`multipart/mixed; boundary=%q`, mixed.Boundary()))
	for key, values := range headers {
		for _, value := range values {
			fmt.Fprintf(&output, "%s: %s\r\n", key, value)
		}
	}
	output.WriteString("\r\n")

	if req.HTMLBody != "" {
		var alternative bytes.Buffer
		alt := multipart.NewWriter(&alternative)
		writeTextPart(alt, "text/plain; charset=utf-8", req.TextBody)
		writeTextPart(alt, "text/html; charset=utf-8", sanitizeHTML(req.HTMLBody))
		_ = alt.Close()
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", fmt.Sprintf(`multipart/alternative; boundary=%q`, alt.Boundary()))
		part, _ := mixed.CreatePart(partHeader)
		_, _ = part.Write(alternative.Bytes())
	} else {
		writeTextPart(mixed, "text/plain; charset=utf-8", req.TextBody)
	}
	if len(req.Attachments) > maxAttachments {
		return nil, fmt.Errorf("message has too many attachments (maximum %d)", maxAttachments)
	}
	for _, attachment := range req.Attachments {
		body, decodeErr := base64.StdEncoding.DecodeString(attachment.ContentBase64)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode attachment %q: %w", attachment.Filename, decodeErr)
		}
		if len(body) > maxComposeAttachmentBytes {
			return nil, fmt.Errorf("attachment %q exceeds 18 MiB", attachment.Filename)
		}
		name := safeFilename(attachment.Filename)
		if name == "" {
			return nil, errors.New("attachment filename is required")
		}
		contentType := attachment.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", contentType+`; name="`+escapeHeaderParam(name)+`"`)
		h.Set("Content-Disposition", `attachment; filename="`+escapeHeaderParam(name)+`"`)
		h.Set("Content-Transfer-Encoding", "base64")
		part, _ := mixed.CreatePart(h)
		encoded := base64.StdEncoding.EncodeToString(body)
		for len(encoded) > 76 {
			_, _ = io.WriteString(part, encoded[:76]+"\r\n")
			encoded = encoded[76:]
		}
		_, _ = io.WriteString(part, encoded+"\r\n")
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	return normalizeCRLF(output.Bytes()), nil
}

func writeTextPart(writer *multipart.Writer, contentType, body string) {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "8bit")
	part, _ := writer.CreatePart(h)
	_, _ = io.WriteString(part, strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
}

func firstAddress(header messageMail.Header, key string) string {
	addresses := addressList(header, key)
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}

func addressList(header messageMail.Header, key string) []string {
	addresses, err := header.AddressList(key)
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address == nil {
			continue
		}
		result = append(result, address.String())
	}
	return result
}

func normalizeAddress(value string) string {
	address, err := mail.ParseAddress(value)
	if err == nil {
		return strings.ToLower(address.Address)
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "<>"))
}

func normalizeAddresses(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized := normalizeAddress(value)
		if normalized != "" && !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	sort.Strings(result)
	return result
}

func validateRecipients(values []string) error {
	if len(values) == 0 {
		return errors.New("at least one recipient is required")
	}
	if len(values) > 100 {
		return errors.New("too many recipients")
	}
	for _, value := range values {
		if _, err := mail.ParseAddress(value); err != nil {
			return fmt.Errorf("invalid recipient %q", value)
		}
	}
	return nil
}

func formatAddressHeader(values []string) string {
	formatted := make([]string, 0, len(values))
	for _, value := range values {
		if address, err := mail.ParseAddress(value); err == nil {
			formatted = append(formatted, address.String())
		}
	}
	return strings.Join(formatted, ", ")
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("message part exceeds parsing limit")
	}
	return data, nil
}

func sanitizeHTML(value string) string {
	policy := bluemonday.UGCPolicy()
	policy.AllowAttrs("style").OnElements("p", "span", "div", "table", "td", "th")
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	policy.AddTargetBlankToFullyQualifiedLinks(true)
	clean := policy.Sanitize(value)
	clean = regexp.MustCompile(`(?i)<img[^>]+src=["']https?://[^>]*>`).ReplaceAllString(clean, "")
	return clean
}

func htmlToText(value string) string {
	withoutTags := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(value, " ")
	return strings.TrimSpace(whitespace.ReplaceAllString(html.UnescapeString(withoutTags), " "))
}

func snippet(value string) string {
	value = whitespace.ReplaceAllString(strings.TrimSpace(value), " ")
	if utf8.RuneCountInString(value) <= 180 {
		return value
	}
	runes := []rune(value)
	return string(runes[:180]) + "…"
}

func threadKey(subject, references, inReplyTo string) string {
	base := strings.ToLower(strings.TrimSpace(subject))
	for {
		trimmed := regexp.MustCompile(`^(re|fw|fwd)\s*:\s*`).ReplaceAllString(base, "")
		if trimmed == base {
			break
		}
		base = trimmed
	}
	if references != "" {
		base += "|" + strings.Fields(references)[0]
	}
	if references == "" && inReplyTo != "" {
		base += "|" + strings.Fields(inReplyTo)[0]
	}
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:16])
}

func cleanHeader(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
}
func cleanMessageID(value string) string {
	value = cleanHeader(value)
	if !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") || strings.Contains(value, " ") {
		return ""
	}
	return value
}
func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	if value == "." {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	return strings.TrimSpace(value)
}
func extensionForType(mediaType string) string {
	if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
func escapeHeaderParam(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\\", "_"), `"`, "_")
}
func normalizeCRLF(value []byte) []byte {
	value = bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
	value = bytes.ReplaceAll(value, []byte("\r"), []byte("\n"))
	return bytes.ReplaceAll(value, []byte("\n"), []byte("\r\n"))
}
