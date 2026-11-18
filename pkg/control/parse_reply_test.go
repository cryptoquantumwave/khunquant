package control

import "testing"

func TestReplyDecision_ApproveWords(t *testing.T) {
	words := []string{"yes", "y", "ok", "okay", "approve", "ใช่", "ตกลง", "โอเค", "อนุมัติ"}
	for _, w := range words {
		for _, v := range []string{w, "  " + w + "  ", "\t" + w + "\n", w + ".", w + "!", w + "?", w + "!!", w + " !", w + "。", w + "！", w + "？"} {
			if approve, ok := replyDecision(v); !ok || !approve {
				t.Errorf("replyDecision(%q) = (%v, %v), want (true, true)", v, approve, ok)
			}
		}
	}
	for _, v := range []string{"YES", "Yes", "yEs", "OK", "Okay", "APPROVE", "Y"} {
		if approve, ok := replyDecision(v); !ok || !approve {
			t.Errorf("replyDecision(%q) = (%v, %v), want (true, true)", v, approve, ok)
		}
	}
}

func TestReplyDecision_RejectWords(t *testing.T) {
	words := []string{"no", "n", "reject", "ไม่", "ไม่ใช่", "ปฏิเสธ"}
	for _, w := range words {
		for _, v := range []string{w, "  " + w + "  ", "\t" + w + "\n", w + ".", w + "!", w + "?", w + "??", w + "。"} {
			if approve, ok := replyDecision(v); !ok || approve {
				t.Errorf("replyDecision(%q) = (%v, %v), want (false, true)", v, approve, ok)
			}
		}
	}
	for _, v := range []string{"NO", "No", "N", "REJECT", "Reject"} {
		if approve, ok := replyDecision(v); !ok || approve {
			t.Errorf("replyDecision(%q) = (%v, %v), want (false, true)", v, approve, ok)
		}
	}
}

func TestReplyDecision_NotDecisions(t *testing.T) {
	for _, v := range []string{
		"", "   ", "!", "?!", ".",
		"yes but half", "yes, but only half", "ok later", "ok then", "yes please", "no thanks",
		"ใช่ แต่ครึ่งเดียว", "ไม่แน่ใจ", "ตกลงนะ", "ไม่ใช่ตอนนี้",
		"yesterday", "no way", "yess", "okey", "nope", "yup", "approved", "rejected", "nn",
		"yes\nno", "y e s",
		"👍", "👎", "✅", "❌", "👍👍",
		"/approve ABCD", "yes?no",
	} {
		if approve, ok := replyDecision(v); ok {
			t.Errorf("replyDecision(%q) = (%v, true), want not a decision", v, approve)
		}
	}
}

func TestClassify_ReplyWordsNeedAReply(t *testing.T) {
	if _, ok := classify(Input{Text: "ใช่"}); ok {
		t.Error("a Thai yes that is not a reply was classified as a command")
	}
	if c, ok := classify(Input{Text: "ไม่", ReplyToMessageID: "5"}); !ok || c.name != cmdNo {
		t.Errorf("classify(ไม่ reply) = %+v, %v, want cmdNo", c, ok)
	}
	if c, ok := classify(Input{Text: "ใช่", ReplyToMessageID: "5"}); !ok || c.name != cmdYes {
		t.Errorf("classify(ใช่ reply) = %+v, %v, want cmdYes", c, ok)
	}
	if _, ok := classify(Input{Text: "ใช่", ReplyToMessageID: "5", Forwarded: true}); ok {
		t.Error("a forwarded yes was classified as a command")
	}
}
