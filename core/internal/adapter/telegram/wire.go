// Package telegram carries traffic between Telegram and the gateway.
//
// The second platform, and the reason for it is not Telegram: it is whether
// the gateway abstraction survives contact with a service that works
// differently. Discord and Telegram disagree about enough to be worth the
// exercise — a thread is not a channel, an edit is not a rewrite of anything
// the user can see, and a mention is a byte range rather than a token.
package telegram

// update is one thing that happened, as long polling returns it.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	From      *user  `json:"from"`
	Chat      chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`

	// Entities mark up ranges of the text: a mention is one of these rather
	// than a token in the text, because the same @name is a mention only when
	// Telegram says it is.
	Entities []entity `json:"entities"`

	// ReplyToMessage is set when this answers another, which is the closest
	// thing to a thread that a group chat has.
	ReplyToMessage *message `json:"reply_to_message"`

	Document *document `json:"document"`
	Photo    []photo   `json:"photo"`
}

type user struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

type chat struct {
	ID int64 `json:"id"`

	// Type is private, group, supergroup or channel. A private chat has no
	// other readers, which is the same distinction a console binding makes.
	Type  string `json:"type"`
	Title string `json:"title"`
}

type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type photo struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// response is the envelope every method answers with.
type response[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`

	Parameters *responseParameters `json:"parameters"`
}

// responseParameters is what Telegram says about a refusal beyond describing
// it.
type responseParameters struct {
	// RetryAfter is how long to wait, which Telegram states here rather than
	// leaving to a header.
	RetryAfter int `json:"retry_after"`
}

// sentMessage is what sendMessage and editMessageText return.
type sentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      chat  `json:"chat"`
}

// self is what getMe returns, which is how the bot learns its own name.
type self struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
