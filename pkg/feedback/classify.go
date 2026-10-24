package feedback

import (
	"strings"
	"unicode"
)

// The classes of a reply. Only the class is ever stored; the text is not.
const (
	ReplyAffirmative = "affirmative"
	ReplyNegative    = "negative"
	ReplyQuestion    = "question"
	ReplyOther       = "other"
)

// maxClassifyRunes bounds the text looked at: a long reply is read by its start.
const maxClassifyRunes = 400

// Keyword lists, English and Thai. English words are matched as whole words; Thai is
// written without spaces, so its terms are matched as substrings. The lists are short
// and meant to be cheap: a reply none of them recognises is "other".
var (
	questionWordsEN = []string{"what", "why", "how", "when", "where", "who", "which", "is", "are", "can", "could",
		"should", "do", "does", "will", "would"}
	questionTermsTH = []string{"ทำไม", "อะไร", "ยังไง", "อย่างไร", "เมื่อไหร่", "เมื่อไร", "ที่ไหน", "ใคร", "ไหม", "มั้ย", "หรือเปล่า", "หรือไม่", "เหรอ"}

	negativeWordsEN = []string{"no", "nope", "not", "dont", "don't", "stop", "ignore", "wrong", "never", "useless", "noise", "spam", "mute", "quiet", "enough"}
	negativeTermsTH = []string{"ไม่", "หยุด", "เลิก", "ผิด", "รก", "รำคาญ", "พอแล้ว", "ไม่ต้อง"}

	affirmativeWordsEN = []string{"yes", "yep", "yeah", "ok", "okay", "sure", "thanks", "thank", "thx", "good", "great", "nice", "done", "agree", "approved", "right", "correct"}
	affirmativeTermsTH = []string{"ใช่", "โอเค", "ตกลง", "ขอบคุณ", "ได้", "ดี", "เอา", "รับทราบ", "ถูกต้อง", "เห็นด้วย"}
)

// ClassifyReply sorts the text of a reply into affirmative, negative, question or
// other. It is code, not a model, and it decides in this order: a question mark or a
// question word first; then negation (so "not ok" is negative); then agreement.
func ClassifyReply(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if r := []rune(t); len(r) > maxClassifyRunes {
		t = string(r[:maxClassifyRunes])
	}
	if t == "" {
		return ReplyOther
	}
	words := strings.FieldsFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' })
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	hasWord := func(list []string) bool {
		for _, w := range list {
			if set[w] {
				return true
			}
		}
		return false
	}
	hasTerm := func(list []string) bool {
		for _, w := range list {
			if strings.Contains(t, w) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.ContainsAny(t, "?？") || (len(words) > 0 && contains(questionWordsEN, words[0])) || hasTerm(questionTermsTH):
		return ReplyQuestion
	case hasWord(negativeWordsEN) || hasTerm(negativeTermsTH):
		return ReplyNegative
	case hasWord(affirmativeWordsEN) || hasTerm(affirmativeTermsTH):
		return ReplyAffirmative
	}
	return ReplyOther
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
