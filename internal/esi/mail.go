package esi

// EVE mail. Read-only: headers arrive 50 per request (paged backwards
// with last_mail_id), bodies are immutable and cache for 30 days.
// Recipients of type mailing_list must NOT go into /universe/names/ —
// list names come from /characters/{id}/mail/lists/ only.

import (
	"fmt"
	"time"
)

// MailRecipient is one addressee of a mail.
type MailRecipient struct {
	RecipientID   int64  `json:"recipient_id"`
	RecipientType string `json:"recipient_type"` // character|corporation|alliance|mailing_list
}

// MailHeader is one line of the mail list.
type MailHeader struct {
	MailID     int64           `json:"mail_id"`
	From       int64           `json:"from"`
	IsRead     bool            `json:"is_read"`
	Labels     []int64         `json:"labels"`
	Recipients []MailRecipient `json:"recipients"`
	Subject    string          `json:"subject"`
	Timestamp  time.Time       `json:"timestamp"`
}

// MailHeaders returns up to 50 headers, newest first. labelID narrows
// to one label (0 = everything), beforeMailID pages further back (only
// mails older than that id are returned).
func (c *Client) MailHeaders(characterID, labelID, beforeMailID int64) ([]MailHeader, error) {
	path := fmt.Sprintf("/characters/%d/mail/", characterID)
	sep := "?"
	if labelID != 0 {
		path += fmt.Sprintf("%slabels=%d", sep, labelID)
		sep = "&"
	}
	if beforeMailID != 0 {
		path += fmt.Sprintf("%slast_mail_id=%d", sep, beforeMailID)
	}
	var out []MailHeader
	_, err := c.get(characterID, path, &out)
	return out, err
}

// MailLabel is one folder of the character's mailbox. The four built-in
// labels are 1 Inbox, 2 Sent, 4 [Corp], 8 [Alliance].
type MailLabel struct {
	LabelID     int64  `json:"label_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	UnreadCount int64  `json:"unread_count"`
}

type MailLabels struct {
	Labels      []MailLabel `json:"labels"`
	TotalUnread int64       `json:"total_unread_count"`
}

func (c *Client) MailLabelList(characterID int64) (*MailLabels, error) {
	var out MailLabels
	if _, err := c.get(characterID, fmt.Sprintf("/characters/%d/mail/labels/", characterID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MailingList is a subscribed mailing list; the only place its name exists.
type MailingList struct {
	MailingListID int64  `json:"mailing_list_id"`
	Name          string `json:"name"`
}

func (c *Client) MailingLists(characterID int64) ([]MailingList, error) {
	var out []MailingList
	_, err := c.get(characterID, fmt.Sprintf("/characters/%d/mail/lists/", characterID), &out)
	return out, err
}

// Mail is a full letter. Body is CCP's in-game markup (<br>, font tags,
// showinfo links) — sanitize before rendering.
type Mail struct {
	Body       string          `json:"body"`
	From       int64           `json:"from"`
	Labels     []int64         `json:"labels"`
	Read       bool            `json:"read"`
	Recipients []MailRecipient `json:"recipients"`
	Subject    string          `json:"subject"`
	Timestamp  time.Time       `json:"timestamp"`
}

func (c *Client) MailBody(characterID, mailID int64) (*Mail, error) {
	var out Mail
	if _, err := c.get(characterID, fmt.Sprintf("/characters/%d/mail/%d/", characterID, mailID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
