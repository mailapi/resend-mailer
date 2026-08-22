package main

type EmailAddress struct {
	Email string  `json:"email"`
	Name  *string `json:"name,omitempty"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type OutboundMessageRequest struct {
	From        EmailAddress   `json:"from"`
	To          []EmailAddress `json:"to"`
	Cc          []EmailAddress `json:"cc,omitempty"`
	Bcc         []EmailAddress `json:"bcc,omitempty"`
	ReplyTo     []EmailAddress `json:"replyTo,omitempty"`
	Subject     *string        `json:"subject,omitempty"`
	Text        *string        `json:"text,omitempty"`
	HTML        *string        `json:"html,omitempty"`
	Headers     []Header       `json:"headers,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
}

type MessageAcceptedResponse struct {
	ID string `json:"id"`
}

type Problem struct {
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Status   int            `json:"status"`
	Detail   string         `json:"detail,omitempty"`
	Instance string         `json:"instance,omitempty"`
	Extra    map[string]any `json:"-"`
}
