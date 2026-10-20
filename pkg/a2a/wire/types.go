// PicoClaw - Ultra-lightweight personal AI agent

package wire

// Agent is the public identity view returned by the platform.
type Agent struct {
	ID          string `json:"id"`
	AgentID     string `json:"agent_id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	DID         string `json:"did"`
}

// Message is one direct message.
type Message struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	From           string `json:"from"`
	FromAgent      string `json:"from_agent"`
	To             string `json:"to"`
	ToAgent        string `json:"to_agent"`
	Body           string `json:"body"`
	Content        string `json:"content"`
	Text           string `json:"text"`
	CreatedAt      string `json:"created_at"`
	Signed         bool   `json:"signed"`
	SigState       string `json:"sig_state"`
	Read           bool   `json:"read"`
}

// BodyText returns the message body using whichever alias the platform filled.
func (m Message) BodyText() string {
	if m.Body != "" {
		return m.Body
	}
	if m.Content != "" {
		return m.Content
	}
	return m.Text
}

// Summary is the conversation summary block.
type Summary struct {
	LastMessage   string `json:"last_message"`
	LastMessageAt string `json:"last_message_at"`
	MessageCount  int    `json:"message_count"`
	UnreadCount   int    `json:"unread_count"`
}

// Conversation is one peer conversation.
type Conversation struct {
	ID           string    `json:"id"`
	WithAgent    Agent     `json:"with_agent"`
	Status       string    `json:"status"`
	YouInitiated bool      `json:"you_initiated"`
	UnreadCount  int       `json:"unread_count"`
	MessageCount int       `json:"message_count"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
	LastMessage  *Message  `json:"last_message"`
	Summary      Summary   `json:"summary"`
	Messages     []Message `json:"messages"`
}

// Home is the inbox poll response.
type Home struct {
	Success       bool           `json:"success"`
	Agent         Agent          `json:"agent"`
	Conversations []Conversation `json:"conversations"`
	UnreadTotal   int            `json:"unread_total"`
	ServerTime    string         `json:"server_time"`
}

// ConversationDetail is the single-conversation response.
type ConversationDetail struct {
	Success       bool         `json:"success"`
	Conversation  Conversation `json:"conversation"`
	Messages      []Message    `json:"messages"`
	UnreadCleared int          `json:"unread_cleared"`
}

// SendRequest is the POST /a2a/{peer} body. Content is signed as-is.
type SendRequest struct {
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	Sig       string `json:"sig"`
}

// SendResponse is the successful send response.
type SendResponse struct {
	Success        bool   `json:"success"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	From           string `json:"from"`
	To             string `json:"to"`
	Delivered      bool   `json:"delivered"`
	Signed         bool   `json:"signed"`
	SigState       string `json:"sig_state"`
	SignMode       string `json:"sign_mode"`
	CreatedAt      string `json:"created_at"`
}

// ErrorResponse is the platform's error body.
type ErrorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}
