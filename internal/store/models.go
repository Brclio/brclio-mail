package store

import "time"

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusActive    = "active"
	StatusSuspended = "suspended"

	MailboxInbox   = "INBOX"
	MailboxStarred = "Starred"
	MailboxDrafts  = "Drafts"
	MailboxSent    = "Sent"
	MailboxArchive = "Archive"
	MailboxJunk    = "Junk"
	MailboxTrash   = "Trash"
)

var SystemMailboxes = []string{MailboxInbox, MailboxDrafts, MailboxSent, MailboxArchive, MailboxJunk, MailboxTrash}

type User struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	DisplayName         string     `json:"displayName"`
	Role                string     `json:"role"`
	Status              string     `json:"status"`
	AuthVersion         int64      `json:"-"`
	ProtocolAuthVersion int64      `json:"-"`
	QuotaBytes          int64      `json:"quotaBytes"`
	UsedBytes           int64      `json:"usedBytes"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	LastLoginAt         *time.Time `json:"lastLoginAt,omitempty"`
	PasswordHash        string     `json:"-"`
	DomainID            string     `json:"domainId"`
	LocalPart           string     `json:"localPart"`
}

type Domain struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	Verification   string     `json:"verificationToken"`
	DKIMSelector   string     `json:"dkimSelector"`
	DKIMPublicKey  string     `json:"dkimPublicKey"`
	DKIMPrivateKey string     `json:"-"`
	CreatedAt      time.Time  `json:"createdAt"`
	VerifiedAt     *time.Time `json:"verifiedAt,omitempty"`
}

type Alias struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	Target    string    `json:"target"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

type Message struct {
	ID              string     `json:"id"`
	RFCMessageID    string     `json:"messageId"`
	ThreadKey       string     `json:"threadKey"`
	EnvelopeFrom    string     `json:"envelopeFrom"`
	EnvelopeTo      []string   `json:"envelopeTo"`
	HeaderFrom      string     `json:"from"`
	HeaderTo        []string   `json:"to"`
	HeaderCC        []string   `json:"cc"`
	HeaderBCC       []string   `json:"bcc,omitempty"`
	ReplyTo         string     `json:"replyTo,omitempty"`
	Subject         string     `json:"subject"`
	TextBody        string     `json:"textBody"`
	HTMLBody        string     `json:"htmlBody,omitempty"`
	Snippet         string     `json:"snippet"`
	Raw             []byte     `json:"-"`
	SizeBytes       int64      `json:"sizeBytes"`
	AttachmentCount int        `json:"attachmentCount"`
	Direction       string     `json:"direction"`
	TransportStatus string     `json:"transportStatus"`
	SentAt          *time.Time `json:"sentAt,omitempty"`
	ReceivedAt      time.Time  `json:"receivedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	UserMailbox     string     `json:"mailbox,omitempty"`
	UserFlags       []string   `json:"flags,omitempty"`
	UserDeletedAt   *time.Time `json:"userDeletedAt,omitempty"`
	UserExpungedAt  *time.Time `json:"userExpungedAt,omitempty"`
	MailboxEntryID  string     `json:"mailboxEntryId,omitempty"`
	UID             uint32     `json:"uid,omitempty"`
}

type Attachment struct {
	ID          string `json:"id"`
	MessageID   string `json:"messageId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	ContentID   string `json:"contentId,omitempty"`
	Disposition string `json:"disposition"`
	SizeBytes   int64  `json:"sizeBytes"`
	SHA256      string `json:"sha256"`
	Content     []byte `json:"-"`
}

type QueueItem struct {
	ID          string     `json:"id"`
	MessageID   string     `json:"messageId"`
	Recipient   string     `json:"recipient"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	NextAttempt time.Time  `json:"nextAttempt"`
	LastError   string     `json:"lastError,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type AuditEvent struct {
	ID         string    `json:"id"`
	ActorID    string    `json:"actorId,omitempty"`
	ActorEmail string    `json:"actorEmail,omitempty"`
	Action     string    `json:"action"`
	TargetType string    `json:"targetType"`
	TargetID   string    `json:"targetId"`
	Reason     string    `json:"reason,omitempty"`
	Metadata   string    `json:"metadata,omitempty"`
	IP         string    `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Session struct {
	ID          string
	UserID      string
	TokenHash   string
	AuthVersion int64
	ExpiresAt   time.Time
}

type AppPassword struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	SecretHash string     `json:"-"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

type MailboxSummary struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Unread int64  `json:"unread"`
}

type SystemStats struct {
	Domains               int64 `json:"domains"`
	Users                 int64 `json:"users"`
	ActiveUsers           int64 `json:"activeUsers"`
	Messages              int64 `json:"messages"`
	UserCopies            int64 `json:"userCopies"`
	ArchivedBytes         int64 `json:"archivedBytes"`
	EstimatedStorageBytes int64 `json:"estimatedStorageBytes"`
	Queued                int64 `json:"queued"`
	Failed                int64 `json:"failed"`
}
